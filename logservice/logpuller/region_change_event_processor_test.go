// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package logpuller

import (
	"context"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/cdcpb"
	"github.com/pingcap/ticdc/heartbeatpb"
	"github.com/pingcap/ticdc/logservice/logpuller/regionlock"
	"github.com/pingcap/tiflow/pkg/security"
	"github.com/pingcap/tiflow/pkg/spanz"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/tikv"
)

func newSubscriptionClientForTestRegionChangeEventProcessor(eventCh chan LogEvent) *SubscriptionClient {
	// only requires `SubscriptionClient.onRegionFail`.
	clientConfig := &SubscriptionClientConfig{
		RegionRequestWorkerPerStore:   1,
		ChangeEventProcessorNum:       32,
		AdvanceResolvedTsIntervalInMs: 300,
	}
	client := NewSubscriptionClient(ClientIDTest, clientConfig, nil, nil, nil, nil, &security.Credential{})
	client.consume = func(ctx context.Context, e LogEvent) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case eventCh <- e:
			return nil
		}
	}
	return client
}

// For UPDATE SQL, its prewrite event has both value and old value.
// It is possible that TiDB prewrites multiple times for the same row when
// there are other transactions it conflicts with. For this case,
// if the value is not "short", only the first prewrite contains the value.
//
// TiKV may output events for the UPDATE SQL as following:
//
// TiDB: [Prwrite1]    [Prewrite2]      [Commit]
//
//	v             v                v                                   Time
//
// ---------------------------------------------------------------------------->
//
//	^            ^    ^           ^     ^       ^     ^          ^     ^
//
// TiKV:   [Scan Start] [Send Prewrite2] [Send Commit] [Send Prewrite1] [Send Init]
// TiCDC:                    [Recv Prewrite2]  [Recv Commit] [Recv Prewrite1] [Recv Init]
func TestHandleEventEntryEventOutOfOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventCh := make(chan LogEvent, 2)
	client := newSubscriptionClientForTestRegionChangeEventProcessor(eventCh)

	defer client.Close(ctx)

	processor := newChangeEventProcessor(1, client)

	span := heartbeatpb.TableSpan{
		StartKey: spanz.ToComparableKey([]byte{}), // TODO: remove spanz dependency
		EndKey:   spanz.ToComparableKey(spanz.UpperBoundKey),
	}
	region := newRegionInfo(
		tikv.RegionVerID{},
		span,
		&tikv.RPCContext{},
		&subscribedSpan{subID: SubscriptionID(1)},
	)
	region.lockedRangeState = &regionlock.LockedRangeState{}
	state := newRegionFeedState(region, 1)
	state.start()

	// Receive prewrite2 with empty value.
	events := &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{{
				StartTs:  1,
				Type:     cdcpb.Event_PREWRITE,
				OpType:   cdcpb.Event_Row_PUT,
				Key:      []byte("key"),
				Value:    nil,
				OldValue: []byte("oldvalue"),
			}},
		},
	}
	err := processor.handleEventEntry(ctx, events, state)
	require.Nil(t, err)

	// Receive commit.
	events = &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{{
				StartTs:  1,
				CommitTs: 2,
				Type:     cdcpb.Event_COMMIT,
				OpType:   cdcpb.Event_Row_PUT,
				Key:      []byte("key"),
			}},
		},
	}
	err = processor.handleEventEntry(context.Background(), events, state)
	require.Nil(t, err)

	// Must not output event.
	select {
	case <-eventCh:
		require.True(t, false, "shouldn't get an event")
	case <-time.NewTimer(100 * time.Millisecond).C:
	}

	// Receive prewrite1 with actual value.
	events = &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{{
				StartTs:  1,
				Type:     cdcpb.Event_PREWRITE,
				OpType:   cdcpb.Event_Row_PUT,
				Key:      []byte("key"),
				Value:    []byte("value"),
				OldValue: []byte("oldvalue"),
			}},
		},
	}
	err = processor.handleEventEntry(ctx, events, state)
	require.Nil(t, err)

	// Must not output event.
	select {
	case <-eventCh:
		require.True(t, false, "shouldn't get an event")
	case <-time.NewTimer(100 * time.Millisecond).C:
	}

	// Receive initialized.
	events = &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{
				{
					Type: cdcpb.Event_INITIALIZED,
				},
			},
		},
	}
	err = processor.handleEventEntry(ctx, events, state)
	require.Nil(t, err)

	// Must output event.
	select {
	case event := <-eventCh:
		require.Equal(t, uint64(2), event.Val.CRTs)
		require.Equal(t, uint64(1), event.Val.StartTs)
		require.Equal(t, "value", string(event.Val.Value))
		require.Equal(t, "oldvalue", string(event.Val.OldValue))
	case <-time.NewTimer(100 * time.Millisecond).C:
		require.True(t, false, "must get an event")
	}
}

func TestHandleResolvedTs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventCh := make(chan LogEvent, 2)
	client := newSubscriptionClientForTestRegionChangeEventProcessor(eventCh)
	defer client.Close(ctx)

	processor := newChangeEventProcessor(1, client)

	s1 := newRegionFeedState(regionInfo{verID: tikv.NewRegionVerID(1, 1, 1)}, 1)
	s1.region.subscribedSpan = client.newSubscribedSpan(1, heartbeatpb.TableSpan{}, 0)
	s1.region.lockedRangeState = &regionlock.LockedRangeState{}
	s1.setInitialized()
	s1.updateResolvedTs(9)

	s2 := newRegionFeedState(regionInfo{verID: tikv.NewRegionVerID(2, 2, 2)}, 2)
	s2.region.subscribedSpan = client.newSubscribedSpan(2, heartbeatpb.TableSpan{}, 0)
	s2.region.lockedRangeState = &regionlock.LockedRangeState{}
	s2.setInitialized()
	s2.updateResolvedTs(11)

	s3 := newRegionFeedState(regionInfo{verID: tikv.NewRegionVerID(3, 3, 3)}, 3)
	s3.region.subscribedSpan = client.newSubscribedSpan(3, heartbeatpb.TableSpan{}, 0)
	s3.region.lockedRangeState = &regionlock.LockedRangeState{}
	s3.updateResolvedTs(8)

	processor.handleResolvedTs(ctx, resolvedTsBatch{ts: 10, regions: []*regionFeedState{s1, s2, s3}})
	require.Equal(t, uint64(10), s1.getLastResolvedTs())
	require.Equal(t, uint64(11), s2.getLastResolvedTs())
	require.Equal(t, uint64(8), s3.getLastResolvedTs())
}
