//go:build race

package bench

// raceEnabled is true when the binary is built with the race detector. The
// wall-clock throughput and latency gates measure timing ratios that race
// instrumentation distorts unevenly (the backend's many instrumented calls
// slow more than the raw baseline's tight loop), so those gates skip under
// -race. The deterministic allocation gate still runs. perf.yml exercises the
// timing gates without -race.
const raceEnabled = true
