package server

import "testing"

// BenchmarkTaskSeal measures minting a task id for an 8-byte handler id, a
// typical short job id.
func BenchmarkTaskSeal(b *testing.B) {
	s := newTaskSealer(nil, "svc")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.seal("job-0007", "alice"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTaskOpen measures checking a task id that passes every check, the
// path a polling client takes on each tasks/get.
func BenchmarkTaskOpen(b *testing.B) {
	s := newTaskSealer(nil, "svc")
	id, err := s.seal("job-0007", "alice")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.open(id, "alice"); err != nil {
			b.Fatal(err)
		}
	}
}
