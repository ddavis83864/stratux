angular.module('appControllers').controller('ReadinessCtrl', ReadinessCtrl); // get the main module contollers set
ReadinessCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval']; // Inject my dependencies

// ReadinessCtrl drives the Sentry-style readiness dashboard: a single
// overall state plus one tile per monitored component, polling the new
// /getHealth endpoint. Unlike the Status page's live websocket, health is
// a slower-moving judgment (the backend recomputes it on its own multi-
// second tick - see main/health.go's healthUpdateInterval), so a plain
// polled $http.get is a better fit than another websocket.
function ReadinessCtrl($rootScope, $scope, $state, $http, $interval) {

	$scope.$parent.helppage = 'plates/readiness-help.html';

	// tileClass maps a readiness.ComponentState string to a Bootstrap
	// label class, matching the color rules the backend already encodes:
	// green only for affirmative evidence, amber for degraded/incomplete,
	// red only for a confirmed failure, gray for not-installed/unknown.
	function tileClass(state) {
		switch (state) {
			case 'READY': return 'label-success';
			case 'DEGRADED': return 'label-warning';
			case 'NOT_READY': return 'label-danger';
			case 'NOT_INSTALLED':
			case 'UNKNOWN':
			default: return 'label-default';
		}
	}

	function durationSeconds(nanoseconds) {
		if (!nanoseconds || nanoseconds <= 0) return null;
		return Math.round(nanoseconds / 1e9);
	}

	// formatDuration renders a whole number of seconds as "12s" or "2m 5s" -
	// the shared building block for every "since last X" display on this
	// dashboard, so the same units/thresholds are used everywhere.
	function formatDuration(ageSeconds) {
		var secs = Math.max(0, Math.round(ageSeconds));
		var mins = Math.floor(secs / 60);
		var rem = secs % 60;
		return mins > 0 ? (mins + 'm ' + rem + 's') : (secs + 's');
	}

	// formatFrameAge renders a receiver's last-frame age for display.
	// ageSeconds is h.<band>.LastFrameAgeSeconds, which the backend leaves
	// null exactly when no frame has ever been received this daemon
	// lifetime (readiness.RadioHealth) - never a fabricated zero from an
	// unset timestamp, and never negative or jumped by a wall-clock
	// correction, since it is computed on the monotonic clock. This must
	// never render "last s ago" - a null age always maps to one of the two
	// explicit sentences below instead of interpolating a missing number.
	function formatFrameAge(ageSeconds, totalFrames) {
		if (ageSeconds === null || ageSeconds === undefined) {
			return totalFrames ? 'last reception unavailable' : 'no frames received since startup';
		}
		return 'last ' + formatDuration(ageSeconds) + ' ago';
	}

	// formatAgoOrNull renders a generic "synced/seen X ago" suffix, or null
	// (render nothing) when the backend has never had a value to report -
	// used wherever a null OptionalTime-derived age must not fall back to
	// displaying an empty/zero duration.
	function formatAgoOrNull(ageSeconds) {
		if (ageSeconds === null || ageSeconds === undefined) return null;
		return formatDuration(ageSeconds) + ' ago';
	}

	function refresh() {
		$http.get(URL_HEALTH_GET).then(function (response) {
			var h = response.data;
			$scope.Health = h;
			$scope.OverallClass = tileClass(h.Overall);

			$scope.UAT978Class = tileClass(h.UAT978.State);
			$scope.ES1090Class = tileClass(h.ES1090.State);
			$scope.GPSClass = tileClass(h.GPS.State);
			$scope.TimeClass = tileClass(mapTimeState(h.Time.State));
			$scope.GDL90Class = tileClass(h.GDL90.State);
			$scope.SystemClass = tileClass(h.System.State);
			$scope.StorageClass = tileClass(h.Storage.State);
			$scope.OverlayClass = tileClass(h.TemporaryOverlay.State);
			$scope.AHRSClass = tileClass(h.AHRS.State);
			$scope.BaroClass = tileClass(h.Baro.State);
			$scope.FanClass = tileClass(h.Fan.State);

			// AHRS/Baro LastMeasurementAgeSeconds is null exactly when no
			// valid measurement has ever been produced (readiness.AHRSHealth/
			// BaroHealth) - same null-means-unavailable convention as the
			// radio tiles' LastFrameAgeSeconds, so this must render one of
			// the two explicit sentences below, never an interpolated
			// "last s ago".
			$scope.AHRSAgeText = h.AHRS.LastMeasurementAgeSeconds === null || h.AHRS.LastMeasurementAgeSeconds === undefined
				? 'no attitude solution yet' : 'last update ' + formatDuration(h.AHRS.LastMeasurementAgeSeconds) + ' ago';
			$scope.BaroAgeText = h.Baro.LastMeasurementAgeSeconds === null || h.Baro.LastMeasurementAgeSeconds === undefined
				? 'no measurement yet' : 'last update ' + formatDuration(h.Baro.LastMeasurementAgeSeconds) + ' ago';

			$scope.LastFrameTextUAT = formatFrameAge(h.UAT978.LastFrameAgeSeconds, h.UAT978.TotalFrames);
			$scope.LastFrameTextES = formatFrameAge(h.ES1090.LastFrameAgeSeconds, h.ES1090.TotalFrames);
			$scope.TimeSyncedAgoText = formatAgoOrNull(h.Time.LastSyncSourceAgeSeconds);
			$scope.GPSUpdateAge = durationSeconds(h.GPS.LastUpdateAge);
			// LastNetworkClientActivity is a nullable RFC3339 wall-clock
			// timestamp (readiness.OptionalTime) - shown as an absolute
			// time, not a computed "X ago", so this never depends on
			// reconciling the browser's own clock against the device's.
			$scope.HasNetworkClientActivity = !!h.GDL90.LastNetworkClientActivity;

			$scope.ConnectState = 'Connected';
		}, function () {
			$scope.ConnectState = 'Disconnected';
		});
	}

	// --- Diagnostics ---------------------------------------------------
	$scope.Diagnostics = { busy: false, message: '', bundles: [] };

	function refreshDiagnosticsList() {
		$http.get(URL_DIAGNOSTICS_LIST).then(function (response) {
			$scope.Diagnostics.bundles = response.data || [];
		});
	}

	$scope.generateDiagnostics = function () {
		if ($scope.Diagnostics.busy) return; // prevents overlapping requests from repeated clicks
		$scope.Diagnostics.busy = true;
		$scope.Diagnostics.message = 'Generating...';
		$http.post(URL_DIAGNOSTICS_GENERATE, {}).then(function (response) {
			$scope.Diagnostics.busy = false;
			if (response.data.success) {
				$scope.Diagnostics.message = response.data.partial
					? ('Generated with warnings: ' + response.data.warning)
					: 'Generated ' + response.data.name;
				refreshDiagnosticsList();
			} else {
				$scope.Diagnostics.message = 'Failed: ' + response.data.error;
			}
		}, function (response) {
			$scope.Diagnostics.busy = false;
			$scope.Diagnostics.message = 'Request failed.';
		});
	};

	$scope.downloadDiagnostics = function (name) {
		window.open(URL_DIAGNOSTICS_DOWNLOAD + '?name=' + encodeURIComponent(name), '_blank');
	};

	// --- Recording -------------------------------------------------------
	$scope.Recording = { status: { state: 'idle' }, busy: false, message: '', sessions: [] };

	function refreshRecordingStatus() {
		$http.get(URL_RECORDING_STATUS).then(function (response) {
			$scope.Recording.status = response.data;
		});
	}

	function refreshRecordingList() {
		$http.get(URL_RECORDING_LIST).then(function (response) {
			$scope.Recording.sessions = response.data || [];
		});
	}

	$scope.startRecording = function () {
		if ($scope.Recording.busy) return;
		$scope.Recording.busy = true;
		$http.post(URL_RECORDING_START, {}).then(function (response) {
			$scope.Recording.busy = false;
			$scope.Recording.message = response.data.success ? 'Recording started.' : ('Failed: ' + response.data.error);
			refreshRecordingStatus();
		}, function () {
			$scope.Recording.busy = false;
			$scope.Recording.message = 'Request failed.';
		});
	};

	$scope.stopRecording = function () {
		if ($scope.Recording.busy) return;
		$scope.Recording.busy = true;
		$http.post(URL_RECORDING_STOP, {}).then(function (response) {
			$scope.Recording.busy = false;
			$scope.Recording.message = 'Recording stopped.';
			refreshRecordingStatus();
			refreshRecordingList();
		}, function () {
			$scope.Recording.busy = false;
			$scope.Recording.message = 'Request failed.';
		});
	};

	$scope.exportRecording = function (id) {
		$http.post(URL_RECORDING_EXPORT + '?id=' + encodeURIComponent(id) + '&format=csv', {}).then(function (response) {
			if (response.data.success) {
				window.open(URL_EXPORT_DOWNLOAD + '?name=' + encodeURIComponent(response.data.name), '_blank');
			} else {
				$scope.Recording.message = 'Export failed.';
			}
		});
	};

	$scope.downloadRecording = function (id) {
		window.open(URL_RECORDING_DOWNLOAD + '?id=' + encodeURIComponent(id), '_blank');
	};

	// The Time component uses its own five-value TimeState enum
	// (UNSYNCHRONIZED/NETWORK_SYNCED/GNSS_SYNCED/DEGRADED/INVALID), not
	// ComponentState directly - map it onto the same color rule.
	function mapTimeState(timeState) {
		switch (timeState) {
			case 'GNSS_SYNCED':
			case 'NETWORK_SYNCED':
				return 'READY';
			case 'DEGRADED':
				return 'DEGRADED';
			case 'INVALID':
				return 'NOT_READY';
			default: // UNSYNCHRONIZED
				return 'DEGRADED';
		}
	}

	$scope.ConnectState = 'Connecting';
	refresh();
	refreshDiagnosticsList();
	refreshRecordingStatus();
	refreshRecordingList();
	var interval = $interval(refresh, 5000);
	var recordingInterval = $interval(function () {
		refreshRecordingStatus();
		if ($scope.Recording.status.state !== 'active') refreshRecordingList();
	}, 5000);
	$scope.$on('$destroy', function () {
		$interval.cancel(interval);
		$interval.cancel(recordingInterval);
	});
}
