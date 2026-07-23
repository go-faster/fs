//go:build !race

package bench

// raceEnabled is false in a normal build; see race_on_test.go.
const raceEnabled = false
