package fisbrecorder

// ============================================================================
// SYNTHETIC TEST DATA — NOT A FIELD RECORDING
//
// Every frame in this file is a hand-constructed, bit-exact-but-fabricated
// UAT uplink message, built to exercise this package's and main's decode/
// replay/tower/cache path *before* a field trip, on a bench, with no SDR.
//
// These frames were never received over the air. They must never be:
//   - injected into the live 978 MHz receiver or godump978,
//   - sent to an operational ForeFlight connection,
//   - included in a field-recording evidence bundle,
//   - used as proof of RF reception, tower coverage, or ForeFlight
//     rendering.
//
// Their only legitimate use is testing this harness itself (see
// syntheticfixture_test.go and docs/fisb-field-recorder-design.md's "What
// the PR #51 field kit can and cannot substantiate from a recording"
// table). Any bundle, log, or report that uses these must say "synthetic"
// on every line that could otherwise be mistaken for real reception.
//
// Generation method: hand-encoded per the bit layouts in
// uatparse/uatparse.go's DecodeUplink/decodeInfoFrame/decodeTimeFormat/
// decodeTextFrame and uatparse/nexrad.go's decodeNexradFrame, then
// self-verified by round-tripping each string through the real
// uatparse.New(...).DecodeUplink() and checking the decoded
// lat/lon/text/NEXRAD content matches what was intended - see
// syntheticfixture_test.go's TestSyntheticFixture_SelfDecodesAsExpected,
// which re-runs that same check on every test run so a future uatparse
// change that would silently break this fixture is caught immediately.
// ============================================================================

// SyntheticTowerALat/Lon are the fabricated coordinates the parser derives
// from SyntheticTowerAFrame1/SyntheticTowerANexradFrame2 (both frames carry
// the same lat/lon, i.e. one synthetic tower sending two messages).
const (
	SyntheticTowerALat = 44.0
	SyntheticTowerALon = -93.0
	SyntheticTowerBLat = 38.5
	SyntheticTowerBLon = -77.0
)

// SyntheticTowerAFrame1 is a synthetic uplink message from "tower A"
// carrying one FIS-B generic text product (id 413).
const SyntheticTowerAFrame1 = "+3e93e97bbbba20001580067449e04d93942055090e008538322018961455216050561481048f1150d480b6196a05054d4830c3100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000;ss=42;rs=0"

// SyntheticTowerANexradFrame2 is a second synthetic message from the SAME
// synthetic tower A (same lat/lon as frame 1), carrying one minimal
// RLE-encoded NEXRAD block (product id 63).
const SyntheticTowerANexradFrame2 = "+3e93e97bbbba2000040000fc49f080000008000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000;ss=44;rs=0"

// SyntheticTowerBFrame3 is a synthetic message from a second, distinct
// synthetic tower B (a different lat/lon entirely), carrying its own
// FIS-B generic text product - proves the fixture (and anything replaying
// it) can distinguish two different derived towers, not just repeat one.
const SyntheticTowerBFrame3 = "+36c16d927d262000158006744c504d93942055090e008538322018961455216050561481048f1150d480b0420e05054d4830c3200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000;ss=40;rs=0"

// SyntheticTowerAText/BText are the exact DLAC-decoded text this package
// expects back out of frames 1/3 respectively - what tests assert against.
const (
	SyntheticTowerAText = "SYNTHETIC BENCH FIXTURE TEXT PRODUCT KXYZ TEST 001"
	SyntheticTowerBText = "SYNTHETIC BENCH FIXTURE TEXT PRODUCT KABC TEST 002"
)

// SyntheticFrames is all three fixture messages in a fixed, documented
// order - the sequence a harness test replays: tower A text, tower A
// NEXRAD, tower B text.
var SyntheticFrames = []string{
	SyntheticTowerAFrame1,
	SyntheticTowerANexradFrame2,
	SyntheticTowerBFrame3,
}
