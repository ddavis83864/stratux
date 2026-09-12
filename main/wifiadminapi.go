/*
wifiadminapi.go: wires the pure wifiadmin package into the running
daemon - see docs/wifi-administration-hardening.md for the full design.

Endpoints (all new, none replace or rename the pre-existing
/setSettings, which remains untouched - see that doc's own
"Backward compatibility" section for why the old, unconfirmed path is
deliberately left in place rather than removed):

	GET  /getWifiAdminStatus       - current transaction state (redacted)
	POST /previewWifiAdminSettings - validate + diff + issue apply token
	POST /applyWifiAdminSettings   - consume apply token, apply, issue reconnect token
	POST /confirmWifiAdminReconnection - consume reconnect token, commit
	POST /cancelWifiAdminChange    - cancel a not-yet-applied preview
	POST /rollbackWifiAdminChange  - manual rollback while awaiting confirmation or in recovery

This feature never runs unless an owner explicitly calls
/previewWifiAdminSettings - initWifiAdmin only loads/recovers state,
never mutates live configuration on its own except the one, deliberate
startup-recovery rollback of an abandoned transaction (see
wifiadmin.NewManager's own doc comment).
*/
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/stratux/stratux/power"
	"github.com/stratux/stratux/wifiadmin"
)

var wifiAdminManager *wifiadmin.Manager

const maxWifiAdminRequestBytes = 8192

// connLocalAddrContextKeyType/connLocalAddrContextKey are the private
// context key main/managementinterface.go's own http.Server.ConnContext
// hook uses to stash each accepted connection's own LocalAddr - see that
// hook's own doc comment for why (wifiadmin's path-aware reconnection
// confirmation needs to know which of this device's own addresses a
// request was physically delivered to, which http.Request itself does
// not expose). An unexported type (not a bare string) as the key,
// following the standard library's own documented convention for
// context keys, so this can never collide with a key some other package
// happens to also store under the same context.
type connLocalAddrContextKeyType struct{}

var connLocalAddrContextKey = connLocalAddrContextKeyType{}

// localAddrFromContext returns the local address the current request's
// own underlying connection was accepted on, formatted exactly as
// net.Addr.String() does (host:port) - or "" if the ConnContext hook
// somehow did not run (e.g. a test driving a handler directly without
// going through the real http.Server; httptest.NewRequest's own default
// Context carries none). ConfirmContext.LocalAddr treats "" as a hard
// rejection, never a bypass - see wifiadmin.validateConfirmationPath.
func localAddrFromContext(ctx context.Context) string {
	addr, _ := ctx.Value(connLocalAddrContextKey).(interface{ String() string })
	if addr == nil {
		return ""
	}
	return addr.String()
}

// initWifiAdmin constructs wifiAdminManager - called from main() once
// preflightSessionID (this process's boot-session id, reused from the
// existing preflight subsystem exactly as shutdownManager/configbackup
// already do) is known. Any startup recovery wifiadmin.NewManager
// performs (rolling back an abandoned transaction from before a crash/
// restart) happens synchronously here, before this function returns -
// see that constructor's own doc comment.
func initWifiAdmin() {
	preflightMu.Lock()
	bootSessionID := preflightSessionID
	preflightMu.Unlock()

	mgr, err := wifiadmin.NewManager(bootSessionID, monotonicSeconds, realWifiExecutor{}, filePersistence{}, []wifiadmin.Precondition{
		otaNotBusyPrecondition,
		configBackupNotBusyPrecondition,
		shutdownNotPendingPrecondition,
		recordingNotActivePrecondition,
	})
	if err != nil {
		log.Printf("wifiadmin: could not initialize (falling back to unavailable): %s\n", err)
		return
	}
	wifiAdminManager = mgr
	if status := mgr.Status(); status.LastError != "" {
		log.Printf("wifiadmin: %s\n", status.LastError)
	}

	go wifiAdminDeadlinePoller()
}

// wifiAdminDeadlinePoller periodically calls CheckDeadline with the
// process's own monotonic clock reading - Manager itself holds no timer
// (see its own doc comment); this is the one place that drives it. A
// 5-second period is far tighter than the reconnect deadline itself
// (tens of seconds) needs, so an automatic rollback never lags
// noticeably behind its own deadline, while staying cheap (CheckDeadline
// is a no-op unless a transaction is actually awaiting confirmation).
func wifiAdminDeadlinePoller() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if wifiAdminManager == nil {
			return
		}
		wifiAdminManager.CheckDeadline(monotonicSeconds())
	}
}

// shutdownNotPendingPrecondition blocks a Wi-Fi transaction whenever the
// controlled-shutdown flow has moved past idle - applying a
// connectivity-affecting change while the device is about to power off
// (or already mid-shutdown) is exactly the kind of overlapping,
// undefined-order mutation this project's own established precondition
// pattern exists to prevent.
func shutdownNotPendingPrecondition() error {
	if shutdownManager == nil {
		return nil
	}
	stage, _ := shutdownManager.Status()
	if stage != power.StageIdle && stage != power.StageFailed {
		return errors.New("a controlled shutdown is in progress")
	}
	return nil
}

// recordingNotActivePrecondition blocks a Wi-Fi transaction while a
// manual or automatic recording is active - disrupting network
// connectivity mid-recording is surprising and unnecessary; recordings
// do not depend on Wi-Fi to keep sampling, so this is a purely
// UX/expectation-management guard, not a technical requirement.
func recordingNotActivePrecondition() error {
	recMu.Lock()
	active := recCurrent != nil && recCurrent.State == recordingStateActive
	recMu.Unlock()
	if active {
		return errors.New("a recording is active")
	}
	return nil
}

// wifiAdminTransactionActive reports whether a Wi-Fi administration
// transaction is currently mid-flight (staged/durable/live writes
// applied, or a reconnection confirmation outstanding) - the reverse
// direction of shutdownNotPendingPrecondition/otaNotBusyPrecondition
// above: those make wifiadmin wait for OTA/shutdown, this lets OTA (see
// main/ota.go's otaAdvance, ActionRequestDisable) wait for wifiadmin
// before requesting an overlay-disabled reboot. Without this, a fresh
// OTA install could rip the network out from under an in-progress Wi-Fi
// apply/confirm cycle mid-transaction - a different flavor of the same
// overlapping-undefined-order-mutation problem this project's existing
// preconditions already guard against in the other direction. Idle,
// previewed (nothing live yet), and any terminal/needs-operator stage
// are not "active" - only work that is actually touching the running
// network justifies OTA waiting.
func wifiAdminTransactionActive() bool {
	if wifiAdminManager == nil {
		return false
	}
	switch wifiAdminManager.Status().Stage {
	case wifiadmin.StageApplying, wifiadmin.StageAwaitingReconnection, wifiadmin.StageRollingBack:
		return true
	default:
		return false
	}
}

func newWifiAdminToken(prefix string) func() string {
	return func() string {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("wifiadmin: crypto/rand failed: " + err.Error())
		}
		return prefix + "-" + hex.EncodeToString(b[:])
	}
}

var newWifiAdminApplyToken = newWifiAdminToken("wifiapply")
var newWifiAdminReconnectToken = newWifiAdminToken("wifireconnect")

func wifiAdminStatusCode(err error) int {
	switch {
	case errors.Is(err, wifiadmin.ErrPreconditionFailed),
		errors.Is(err, wifiadmin.ErrAlreadyInProgress),
		errors.Is(err, wifiadmin.ErrCannotCancelNow),
		errors.Is(err, wifiadmin.ErrNotAwaitingConfirm):
		return http.StatusConflict
	case errors.Is(err, wifiadmin.ErrTokenNotFound),
		errors.Is(err, wifiadmin.ErrTokenAlreadyUsed),
		errors.Is(err, wifiadmin.ErrTokenExpired),
		errors.Is(err, wifiadmin.ErrTokenBootSessionChanged),
		errors.Is(err, wifiadmin.ErrTokenConfigChanged),
		errors.Is(err, wifiadmin.ErrTokenStateChanged),
		errors.Is(err, wifiadmin.ErrReconnectTokenBad):
		return http.StatusGone
	case errors.Is(err, wifiadmin.ErrReconnectionPathInvalid),
		errors.Is(err, wifiadmin.ErrReconnectionHealthCheckFailed):
		// Distinct from ErrReconnectTokenBad's 410: the token itself was
		// fine, but the request did not arrive via a network path this
		// confirmation is allowed to trust - see wifiadmin.ConfirmContext's
		// own doc comment.
		return http.StatusForbidden
	case errors.Is(err, wifiadmin.ErrNoPendingPreview):
		return http.StatusNotFound
	default:
		// Includes *wifiadmin.ValidationError and any other rejection
		// (e.g. a checksum/fingerprint marshal failure) - all of which
		// are the client's own request being unacceptable, never a
		// server-side fault.
		return http.StatusBadRequest
	}
}

func handleGetWifiAdminStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if wifiAdminManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "wifiadmin not initialized"})
		return
	}
	writeJSON(w, http.StatusOK, wifiAdminManager.Status())
}

type previewWifiAdminRequest = wifiadmin.Config

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
		return false
	}
	if dec.More() {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "request body must contain exactly one JSON value"})
		return false
	}
	return true
}

func handlePreviewWifiAdminSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if wifiAdminManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "wifiadmin not initialized"})
		return
	}
	var proposed previewWifiAdminRequest
	if !decodeStrictJSON(w, r, maxWifiAdminRequestBytes, &proposed) {
		return
	}
	proposed.SchemaVersion = wifiadmin.CurrentSchemaVersion

	preview, tok, err := wifiAdminManager.Preview(proposed, newWifiAdminApplyToken)
	if err != nil {
		writeJSON(w, wifiAdminStatusCode(err), map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":          true,
		"preview":          preview,
		"applyToken":       tok.Token,
		"expiresInSeconds": tok.ExpiresAtMonotonic - tok.IssuedAtMonotonic,
	})
}

type applyWifiAdminRequest struct {
	ApplyToken string `json:"applyToken"`
}

func handleApplyWifiAdminSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if wifiAdminManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "wifiadmin not initialized"})
		return
	}
	var req applyWifiAdminRequest
	if !decodeStrictJSON(w, r, maxWifiAdminRequestBytes, &req) {
		return
	}
	if req.ApplyToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "missing required field: applyToken"})
		return
	}
	stage, err := wifiAdminManager.Apply(req.ApplyToken, newWifiAdminReconnectToken)
	if err != nil {
		writeJSON(w, wifiAdminStatusCode(err), map[string]interface{}{"success": false, "error": err.Error(), "stage": string(stage)})
		return
	}
	reconnectTok, _ := wifiAdminManager.ReconnectToken()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        true,
		"stage":          string(stage),
		"reconnectToken": reconnectTok,
	})
}

type confirmWifiAdminReconnectionRequest struct {
	ReconnectToken string `json:"reconnectToken"`
}

func handleConfirmWifiAdminReconnectionRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if wifiAdminManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "wifiadmin not initialized"})
		return
	}
	var req confirmWifiAdminReconnectionRequest
	if !decodeStrictJSON(w, r, maxWifiAdminRequestBytes, &req) {
		return
	}
	if req.ReconnectToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "missing required field: reconnectToken"})
		return
	}
	stage, err := wifiAdminManager.ConfirmReconnection(wifiadmin.ConfirmContext{
		Token:      req.ReconnectToken,
		RemoteAddr: r.RemoteAddr,
		LocalAddr:  localAddrFromContext(r.Context()),
	})
	if err != nil {
		writeJSON(w, wifiAdminStatusCode(err), map[string]interface{}{"success": false, "error": err.Error(), "stage": string(stage)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "stage": string(stage)})
}

func handleCancelWifiAdminChangeRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if wifiAdminManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "wifiadmin not initialized"})
		return
	}
	if err := wifiAdminManager.Cancel(); err != nil {
		writeJSON(w, wifiAdminStatusCode(err), map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func handleRollbackWifiAdminChangeRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if wifiAdminManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "wifiadmin not initialized"})
		return
	}
	stage, err := wifiAdminManager.RequestRollback()
	if err != nil {
		writeJSON(w, wifiAdminStatusCode(err), map[string]interface{}{"success": false, "error": err.Error(), "stage": string(stage)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "stage": string(stage)})
}

// wifiAdminDiagnosticsSummary is the bounded, sanitized summary embedded
// in diagnostic bundles - see docs/wifi-administration-hardening.md's
// "Diagnostics" section. Never a passphrase, a token, or a raw
// configuration file.
type wifiAdminDiagnosticsSummary struct {
	Stage         string             `json:"stage"`
	LastResult    string             `json:"lastResult"`
	LastError     string             `json:"lastError,omitempty"`
	LastKnownGood wifiadmin.Redacted `json:"lastKnownGood"`
}

// wifiAdminDiagnosticsSummaryFor never panics and never returns nil - if
// wifiadmin has not been initialized yet (very early startup, or
// initWifiAdmin itself failed), it returns a zero-value, honestly-empty
// summary, matching alertingDiagnosticsSummary/trafficCPADiagnosticsSummary's
// own established convention (a concrete value, never a possibly-nil
// pointer boxed into the diagnostics bundle's interface{} field, which
// would defeat that field's own omitempty).
// wifiAdminStageForPreflight feeds preflight.Input.WifiAdminStage - see
// preflight/checks.go's wifiAdminChecks for how each stage string maps
// to a readiness card. Never panics; returns "" (mapped to "idle"'s own
// Ready/Info card) when wifiadmin has not been initialized yet.
func wifiAdminStageForPreflight() string {
	if wifiAdminManager == nil {
		return ""
	}
	return string(wifiAdminManager.Status().Stage)
}

func wifiAdminDiagnosticsSummaryFor() wifiAdminDiagnosticsSummary {
	if wifiAdminManager == nil {
		return wifiAdminDiagnosticsSummary{}
	}
	s := wifiAdminManager.Status()
	return wifiAdminDiagnosticsSummary{
		Stage:         string(s.Stage),
		LastResult:    string(s.LastResult),
		LastError:     s.LastError,
		LastKnownGood: s.LastKnownGood,
	}
}
