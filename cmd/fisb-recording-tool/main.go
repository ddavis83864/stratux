// Command fisb-recording-tool is a small CLI over the fisbrecorder
// package's Validate/ReplayFrames/Compare functions, for use from the
// field-kit MacBook (or any bench machine) without writing Go code by
// hand each time. It never touches a network connection or hardware
// itself - see fisbrecorder/replay.go's own doc comment on why replay
// through this tool alone can never drive a live device or a production
// network (there is no code path here that could).
//
// Subcommands (flags must come BEFORE the positional directory
// arguments - Go's flag package stops parsing at the first non-flag
// argument, so "replay <dir> -speed=2" is silently treated as two
// positional args, not one positional arg plus a flag):
//
//	fisb-recording-tool validate <sessionDir>
//	fisb-recording-tool replay [-speed=1.0] [-stopAfter=0] <sessionDir>
//	fisb-recording-tool compare <dirA> <dirB>
//	fisb-recording-tool compare-weather <dirA> <dirB>
//
// See docs/fisb-field-recorder-procedure.md for the field kit's own use
// of this tool as part of start->monitor->stop->verify->preserve.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/stratux/stratux/fisbrecorder"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = runValidate(os.Args[2:])
	case "replay":
		err = runReplay(os.Args[2:])
	case "compare":
		err = runCompare(os.Args[2:])
	case "compare-weather":
		err = runCompareWeather(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fisb-recording-tool:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fisb-recording-tool validate <sessionDir>")
	fmt.Fprintln(os.Stderr, "       fisb-recording-tool replay [-speed=1.0] [-stopAfter=0] <sessionDir>")
	fmt.Fprintln(os.Stderr, "       fisb-recording-tool compare <dirA> <dirB>")
	fmt.Fprintln(os.Stderr, "       fisb-recording-tool compare-weather <dirA> <dirB>")
}

// runValidate prints the Result as indented JSON and sets the process
// exit code (via the returned error) to non-zero when the bundle is not
// "valid" - a field script can act on the exit code alone without
// parsing JSON, or parse the JSON for the reasons why.
func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("validate: expected exactly one <sessionDir> argument")
	}
	res := fisbrecorder.Validate(fs.Arg(0))
	printJSON(res)
	switch res.Classification {
	case fisbrecorder.ClassificationValid:
		return nil
	case fisbrecorder.ClassificationPartial:
		return fmt.Errorf("bundle is PARTIAL - see warnings above")
	default:
		return fmt.Errorf("bundle is UNUSABLE - see errors above")
	}
}

// runReplay replays a session's frames.jsonl.gz and prints each frame's
// seq/elapsed/length as it's replayed (not the raw frame bytes - those
// can be large and are already on disk if needed), then prints the final
// ReplayStats as JSON. It calls no handler beyond printing a progress
// line - it does not itself decode frames or produce GDL90/cache output;
// that requires the real daemon's parser (see the package doc's note that
// this tool has no network code and cannot drive a live device).
func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	speed := fs.Float64("speed", 1.0, "pacing multiplier (1.0=original, >1=faster, <=0=as fast as possible)")
	stopAfter := fs.Uint64("stopAfter", 0, "stop after this many frames (0=no limit)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("replay: expected exactly one <sessionDir> argument (got %d: %v) - flags must come BEFORE the directory, e.g. \"replay -speed=2 <sessionDir>\"", fs.NArg(), fs.Args())
	}
	dir := fs.Arg(0)
	stats, err := fisbrecorder.ReplayFrames(dir, func(rec fisbrecorder.FrameRecord) {
		fmt.Printf("seq=%d elapsedNanos=%d frameBytes=%d\n", rec.Seq, rec.ElapsedNanos, len(rec.Frame))
	}, fisbrecorder.ReplayOptions{SpeedMultiplier: *speed, StopAfter: *stopAfter})
	if err != nil {
		return err
	}
	printJSON(stats)
	return nil
}

// runCompare prints the CompareReport as indented JSON. No Normalize
// function is available from the command line yet (byte-for-byte
// comparison only) - see fisbrecorder.Compare's own doc comment on why a
// normalize function needs domain-specific GDL90 field knowledge this
// tool does not have; a mismatch reported here should be treated as real
// until a specific, justified rule explains it away in code, not waved
// off from the CLI.
func runCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("compare: expected exactly two arguments: <dirA> <dirB>")
	}
	rep, err := fisbrecorder.Compare(fs.Arg(0), fs.Arg(1), nil)
	if err != nil {
		return err
	}
	printJSON(rep)
	return nil
}

// runCompareWeather prints the WeatherGDL90Report as indented JSON - the
// narrow, product-focused alternative to `compare`'s raw byte-for-byte
// GDL90 comparison: isolates FIS-B/weather-bearing packets (GDL90
// message ID 0x07) per destination connection, so a session-duration
// difference (which inflates non-weather heartbeat/traffic counts, see
// fisbrecorder.CompareWeatherGDL90's own doc comment) cannot dilute the
// result. No normalization is applied here either - see that same doc
// comment for why a genuine byte-for-byte match is the expected,
// provable result for this specific message type.
func runCompareWeather(args []string) error {
	fs := flag.NewFlagSet("compare-weather", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("compare-weather: expected exactly two arguments: <dirA> <dirB>")
	}
	rep, err := fisbrecorder.CompareWeatherGDL90(fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	printJSON(rep)
	for _, cc := range rep.PerConnection {
		if len(cc.Mismatched) > 0 || cc.OnlyInA > 0 || cc.OnlyInB > 0 {
			return fmt.Errorf("compare-weather: %s: %d mismatched, %d only-in-A, %d only-in-B weather packets - see the report above", cc.ConnectionKey, len(cc.Mismatched), cc.OnlyInA, cc.OnlyInB)
		}
	}
	return nil
}

func printJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
