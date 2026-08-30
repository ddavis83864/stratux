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

	// clientClass maps the honest, evidence-scoped ClientObservabilityState
	// enum (DETECTED/NOT_DETECTED/UNKNOWN/UNSUPPORTED) onto the same color
	// rule - UNSUPPORTED and UNKNOWN both render as neutral gray, since
	// neither is a claim of failure, just "cannot tell".
	function clientClass(state) {
		switch (state) {
			case 'DETECTED': return 'label-success';
			case 'NOT_DETECTED': return 'label-default';
			case 'UNKNOWN':
			case 'UNSUPPORTED':
			default: return 'label-default';
		}
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
			$scope.ClientClass = clientClass(h.GDL90.ForeFlightDetection.State);

			$scope.LastFrameAgeUAT = durationSeconds(h.UAT978.LastFrameAge);
			$scope.LastFrameAgeES = durationSeconds(h.ES1090.LastFrameAge);
			$scope.TimeSourceAge = durationSeconds(h.Time.LastSyncSourceAge);
			$scope.GPSUpdateAge = durationSeconds(h.GPS.LastUpdateAge);

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
