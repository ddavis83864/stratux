angular.module('appControllers').controller('PowerCtrl', PowerCtrl);
PowerCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval'];

// PowerCtrl drives the Power page: the debounced power/thermal-health
// reading, the previous-session marker, and two manual, confirmed action
// flows - a two-step controlled shutdown and a single-confirmation
// controlled restart. See main/powerapi.go and
// docs/power-shutdown-resilience.md. This is the single supported place
// in the Web UI to restart or shut down the device - the Settings page's
// former standalone Reboot/Shutdown buttons were removed in favor of it.
//
// Neither action ever happens on this page's own initiative. Shutdown
// requires an explicit Prepare Shutdown click followed by an explicit,
// separately confirmed Confirm Shutdown click; Restart requires one
// explicit confirmation (Restart, then Confirm Restart). Either flow can
// be safely abandoned at its confirmation step - the issued token simply
// expires unused server-side.
function PowerCtrl($rootScope, $scope, $state, $http, $interval) {

	$scope.Health = null;
	$scope.HealthError = '';

	$scope.Shutdown = {
		stage: 'idle',
		token: null,
		busy: false,
		error: '',
		confirmChecked: false,
		done: false
	};

	$scope.Restart = {
		stage: 'idle',
		token: null,
		busy: false,
		error: '',
		done: false
	};

	// severityClass maps power.Severity onto the same Bootstrap label
	// classes the Readiness/Preflight pages already use for their own
	// state vocabularies.
	$scope.severityClass = function (severity) {
		switch (severity) {
			case 'critical': return 'label-danger';
			case 'warning': return 'label-warning';
			case 'ok': return 'label-success';
			default: return 'label-default';
		}
	};

	function refreshHealth() {
		$http.get(URL_POWER_HEALTH_GET).then(function (response) {
			$scope.Health = response.data;
			$scope.HealthError = '';
		}, function () {
			$scope.HealthError = 'Could not reach the power-health API.';
		});
	}

	function refreshShutdownStatus() {
		$http.get(URL_SHUTDOWN_STATUS_GET).then(function (response) {
			$scope.Shutdown.stage = response.data.stage;
		});
	}

	// prepareShutdown is step one: it never mutates system state, and can
	// be clicked again at any time (e.g. if the confirmation dialog is
	// dismissed and reopened, or the token is close to expiring) - each
	// call simply issues a fresh token.
	$scope.prepareShutdown = function () {
		$scope.Shutdown.busy = true;
		$scope.Shutdown.error = '';
		$scope.Shutdown.confirmChecked = false;
		$http.post(URL_SHUTDOWN_REQUEST, {}).then(function (response) {
			$scope.Shutdown.busy = false;
			$scope.Shutdown.token = response.data.token;
			$scope.Shutdown.stage = 'confirmation_required';
		}, function (response) {
			$scope.Shutdown.busy = false;
			var data = response.data || {};
			$scope.Shutdown.error = data.error || 'Could not prepare a shutdown request.';
		});
	};

	function refreshRestartStatus() {
		$http.get(URL_REBOOT_STATUS_GET).then(function (response) {
			$scope.Restart.stage = response.data.stage;
		});
	}

	$scope.cancelShutdown = function () {
		$scope.Shutdown.token = null;
		$scope.Shutdown.confirmChecked = false;
		$scope.Shutdown.error = '';
		// The issued token is simply left to expire server-side - there is
		// nothing to undo, since prepareShutdown never mutated anything.
		refreshShutdownStatus();
	};

	// confirmShutdown is step two - the only call in this whole page that
	// actually results in the device powering off, and only once both
	// prepareShutdown has already succeeded and the operator has
	// separately checked the explicit confirmation checkbox below.
	$scope.confirmShutdown = function () {
		if (!$scope.Shutdown.token || !$scope.Shutdown.confirmChecked || $scope.Shutdown.busy) {
			return;
		}
		$scope.Shutdown.busy = true;
		$scope.Shutdown.error = '';
		$http.post(URL_SHUTDOWN_CONFIRM, {token: $scope.Shutdown.token}).then(function (response) {
			$scope.Shutdown.busy = false;
			$scope.Shutdown.done = true;
			$scope.Shutdown.stage = response.data.stage;
		}, function (response) {
			$scope.Shutdown.busy = false;
			var data = response.data || {};
			$scope.Shutdown.error = data.error || 'Shutdown could not be confirmed.';
			$scope.Shutdown.stage = data.stage || $scope.Shutdown.stage;
		});
	};

	// prepareRestart issues a confirmation token immediately when Restart
	// is clicked - never mutates system state, so it is safe to call
	// again (e.g. if a precondition failure is fixed and the operator
	// tries again). The Restart panel's single confirmation only appears
	// once this has already succeeded, so a blocked precondition (an OTA
	// update or configuration restore in progress) is surfaced as an
	// error instead of a confirmation the operator cannot actually use.
	$scope.prepareRestart = function () {
		$scope.Restart.busy = true;
		$scope.Restart.error = '';
		$http.post(URL_REBOOT_REQUEST, {}).then(function (response) {
			$scope.Restart.busy = false;
			$scope.Restart.token = response.data.token;
			$scope.Restart.stage = 'confirmation_required';
		}, function (response) {
			$scope.Restart.busy = false;
			var data = response.data || {};
			$scope.Restart.error = data.error || 'Could not prepare a restart request.';
		});
	};

	$scope.cancelRestart = function () {
		$scope.Restart.token = null;
		$scope.Restart.error = '';
		// The issued token is simply left to expire server-side - there is
		// nothing to undo, since prepareRestart never mutated anything.
		refreshRestartStatus();
	};

	// confirmRestart is the operator's one explicit confirmation - the
	// only call in this whole flow that actually reboots the device, and
	// only once prepareRestart has already succeeded.
	$scope.confirmRestart = function () {
		if (!$scope.Restart.token || $scope.Restart.busy) {
			return;
		}
		$scope.Restart.busy = true;
		$scope.Restart.error = '';
		$http.post(URL_REBOOT_CONFIRM, {token: $scope.Restart.token}).then(function (response) {
			$scope.Restart.busy = false;
			$scope.Restart.done = true;
			$scope.Restart.stage = response.data.stage;
		}, function (response) {
			$scope.Restart.busy = false;
			var data = response.data || {};
			$scope.Restart.error = data.error || 'Restart could not be confirmed.';
			$scope.Restart.stage = data.stage || $scope.Restart.stage;
		});
	};

	refreshHealth();
	refreshShutdownStatus();
	refreshRestartStatus();
	var healthInterval = $interval(refreshHealth, 5000);
	var shutdownInterval = $interval(function () {
		// Once the device has actually been told to power off/restart,
		// there is nothing meaningful left to poll for.
		if (!$scope.Shutdown.done) refreshShutdownStatus();
		if (!$scope.Restart.done) refreshRestartStatus();
	}, 5000);
	$scope.$on('$destroy', function () {
		$interval.cancel(healthInterval);
		$interval.cancel(shutdownInterval);
	});
}
