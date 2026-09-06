angular.module('appControllers').controller('PreflightCtrl', PreflightCtrl);
PreflightCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval'];

// PreflightCtrl drives the simplified Preflight page: a one-screen
// READY/CAUTION/NOT_READY summary built on top of the same /getHealth
// data the detailed Readiness page already shows, plus a small set of
// manual (human-observed) checks the pilot explicitly confirms each
// flight. See main/preflightapi.go and docs/preflight-readiness.md.
//
// This page is purely observational - it never sends a command to any
// other subsystem, and nothing here can affect ADS-B/GDL90 operation.
function PreflightCtrl($rootScope, $scope, $state, $http, $interval) {

	$scope.$parent.helppage = 'plates/readiness-help.html';

	$scope.Preflight = { overall: 'UNKNOWN', summary: '', automated: [], manual: [], profile: {}, disclaimer: '' };
	$scope.Manual = { busy: false, error: '' };
	$scope.ShowDetails = false;
	$scope.ackExpiryHours = 4;
	$scope.ConnectState = 'Connecting';

	// stateClass maps a preflight check's State onto the same Bootstrap
	// label classes the Readiness page already uses for
	// readiness.ComponentState, extended with the two preflight-only
	// values (VERIFY/NOT_APPLICABLE reuse the existing gray "default"
	// treatment; NOT_READY/CAUTION/READY match red/amber/green exactly).
	$scope.stateClass = function (state) {
		switch (state) {
			case 'READY': return 'label-success';
			case 'CAUTION': return 'label-warning';
			case 'NOT_READY': return 'label-danger';
			case 'VERIFY':
			case 'NOT_APPLICABLE':
			case 'UNKNOWN':
			default: return 'label-default';
		}
	};

	// OverallClass mirrors the panel heading's badge to the same three
	// colors the rest of the dashboard uses for an overall rollup.
	function updateOverallClass() {
		$scope.OverallClass = $scope.stateClass($scope.Preflight.overall);
	}

	// checksFor returns every automated check for one component, in the
	// order the backend produced them - used to group the flat
	// Preflight.automated list into the page's fixed section order
	// without the backend needing to know anything about presentation.
	$scope.checksFor = function (component) {
		return ($scope.Preflight.automated || []).filter(function (c) {
			return c.component === component;
		});
	};

	var refreshing = false; // prevents overlapping /getPreflightReport requests
	function refresh() {
		if (refreshing) return;
		refreshing = true;
		$http.get(URL_PREFLIGHT_GET).then(function (response) {
			refreshing = false;
			$scope.Preflight = response.data;
			updateOverallClass();
			$scope.ConnectState = 'Connected';
		}, function () {
			refreshing = false;
			$scope.ConnectState = 'Disconnected';
		});
	}

	$scope.acknowledge = function (checkId) {
		if ($scope.Manual.busy) return;
		$scope.Manual.busy = true;
		$scope.Manual.error = '';
		$http.post(URL_PREFLIGHT_ACK + '?id=' + encodeURIComponent(checkId)).then(function () {
			$scope.Manual.busy = false;
			refresh();
		}, function (response) {
			$scope.Manual.busy = false;
			$scope.Manual.error = (response.data && response.data.error) || 'Could not confirm this check.';
		});
	};

	$scope.clearCheck = function (checkId) {
		if ($scope.Manual.busy) return;
		$scope.Manual.busy = true;
		$scope.Manual.error = '';
		$http.post(URL_PREFLIGHT_CLEAR + '?id=' + encodeURIComponent(checkId)).then(function () {
			$scope.Manual.busy = false;
			refresh();
		}, function (response) {
			$scope.Manual.busy = false;
			$scope.Manual.error = (response.data && response.data.error) || 'Could not clear this check.';
		});
	};

	$scope.resetChecklist = function () {
		if ($scope.Manual.busy) return;
		if (!window.confirm('Reset every manual preflight confirmation? You will need to confirm each one again.')) return;
		$scope.Manual.busy = true;
		$scope.Manual.error = '';
		$http.post(URL_PREFLIGHT_RESET).then(function () {
			$scope.Manual.busy = false;
			refresh();
		}, function (response) {
			$scope.Manual.busy = false;
			$scope.Manual.error = (response.data && response.data.error) || 'Could not reset the checklist.';
		});
	};

	refresh();
	var interval = $interval(refresh, 5000);
	$scope.$on('$destroy', function () {
		$interval.cancel(interval);
	});
}
