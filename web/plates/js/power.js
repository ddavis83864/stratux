angular.module('appControllers').controller('PowerCtrl', PowerCtrl);
PowerCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval'];

// PowerCtrl drives the Power page: the debounced power/thermal-health
// reading, the previous-session marker, and a manual, two-step confirmed
// controlled-shutdown flow. See main/powerapi.go and
// docs/power-shutdown-resilience.md.
//
// This page never shuts the device down on its own initiative - only an
// explicit Prepare Shutdown click followed by an explicit, separately
// confirmed Confirm Shutdown click can ever do that, and either step can
// be safely abandoned (the confirmation token simply expires unused).
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

	refreshHealth();
	refreshShutdownStatus();
	var healthInterval = $interval(refreshHealth, 5000);
	var shutdownInterval = $interval(function () {
		// Once the device has actually been told to power off, there is
		// nothing meaningful left to poll for.
		if (!$scope.Shutdown.done) refreshShutdownStatus();
	}, 5000);
	$scope.$on('$destroy', function () {
		$interval.cancel(healthInterval);
		$interval.cancel(shutdownInterval);
	});
}
