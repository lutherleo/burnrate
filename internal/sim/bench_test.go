package sim_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lutherleo/burnrate/internal/engine"
	"github.com/lutherleo/burnrate/internal/otlp"
	"github.com/lutherleo/burnrate/internal/sim"
	"github.com/lutherleo/burnrate/internal/slo"
)

// BenchmarkReplayIncidentDay measures the whole in-process pipeline on the
// four-day incident-day scenario: protobuf encode + decode of every export,
// OTLP translation, ingestion and one evaluation per minute for the three
// example SLOs. One op is the full four days (5,760 exports).
func BenchmarkReplayIncidentDay(b *testing.B) {
	slos, err := slo.LoadPaths("../../examples/slos")
	if err != nil {
		b.Fatal(err)
	}
	sc, err := sim.Lookup("incident-day")
	if err != nil {
		b.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	for b.Loop() {
		gen, err := sim.NewGenerator(sc, start)
		if err != nil {
			b.Fatal(err)
		}
		eng, err := engine.New(engine.Config{}, slos)
		if err != nil {
			b.Fatal(err)
		}
		tr, err := otlp.NewTranslator(otlp.Options{}, eng.Wants)
		if err != nil {
			b.Fatal(err)
		}
		points := 0
		for {
			req, at, ok := gen.Next()
			if !ok {
				break
			}
			wire, err := proto.Marshal(req)
			if err != nil {
				b.Fatal(err)
			}
			req.Reset()
			if err := proto.Unmarshal(wire, req); err != nil {
				b.Fatal(err)
			}
			samples, res := tr.Translate(req)
			if res.Rejected > 0 {
				b.Fatalf("rejected %v", res.Errors)
			}
			points += len(samples)
			eng.Ingest(samples)
			eng.Advance(at)
		}
		b.ReportMetric(float64(points), "samples/op")
	}
}
