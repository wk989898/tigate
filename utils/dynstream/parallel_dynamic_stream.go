package dynstream

import "fmt"

// Use a hasher to select target stream for the path.
// It implements the DynamicStream interface.
type parallelDynamicStream[A Area, P Path, T Event, D Dest, H Handler[A, P, T, D]] struct {
	pathHaser      PathHasher[P]
	dynamicStreams []*dynamicStreamImpl[A, P, T, D, H]
	feedbackChan   chan Feedback[A, P, D]
}

func newParallelDynamicStream[A Area, P Path, T Event, D Dest, H Handler[A, P, T, D]](streamCount int, hasher PathHasher[P], handler H, option Option) *parallelDynamicStream[A, P, T, D, H] {
	s := &parallelDynamicStream[A, P, T, D, H]{
		pathHaser: hasher,
	}
	if option.EnableMemoryControl {
		s.feedbackChan = make(chan Feedback[A, P, D], 1024)
	}
	for range streamCount {
		s.dynamicStreams = append(s.dynamicStreams, newDynamicStreamImpl(handler, option, s.feedbackChan))
	}
	return s
}

func (s *parallelDynamicStream[A, P, T, D, H]) Start() {
	for _, ds := range s.dynamicStreams {
		ds.Start()
	}
}

func (s *parallelDynamicStream[A, P, T, D, H]) Close() {
	for _, ds := range s.dynamicStreams {
		ds.Close()
	}
}

func (s *parallelDynamicStream[A, P, T, D, H]) hash(path ...P) int {
	if len(path) == 0 {
		panic("no path")
	}
	index := s.pathHaser.HashPath(path[0])
	if index >= len(s.dynamicStreams) {
		panic(fmt.Sprintf("invalid hash result: %v, streams length: %v", index, len(s.dynamicStreams)))
	}
	return index
}

func (s *parallelDynamicStream[A, P, T, D, H]) In(path ...P) chan<- T {
	return s.dynamicStreams[s.hash(path...)].In()
}

func (s *parallelDynamicStream[A, P, T, D, H]) Wake(path ...P) chan<- P {
	return s.dynamicStreams[s.hash(path...)].Wake()
}

func (s *parallelDynamicStream[A, P, T, D, H]) Feedback() <-chan Feedback[A, P, D] {
	return s.feedbackChan
}

func (s *parallelDynamicStream[A, P, T, D, H]) AddPath(path P, dest D, area ...AreaSettings) error {
	return s.dynamicStreams[s.hash(path)].AddPath(path, dest, area...)
}

func (s *parallelDynamicStream[A, P, T, D, H]) RemovePath(path P) error {
	return s.dynamicStreams[s.hash(path)].RemovePath(path)
}

func (s *parallelDynamicStream[A, P, T, D, H]) SetAreaSettings(area A, settings AreaSettings) {
	for _, ds := range s.dynamicStreams {
		ds.SetAreaSettings(area, settings)
	}
}
