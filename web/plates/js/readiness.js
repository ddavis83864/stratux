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

			$scope.LastFrameAgeUAT = durationSeconds(h.UAT978.LastFrameAge);
			$scope.LastFrameAgeES = durationSeconds(h.ES1090.LastFrameAge);
			$scope.TimeSourceAge = durationSeconds(h.Time.LastSyncSourceAge);
			$scope.GPSUpdateAge = durationSeconds(h.GPS.LastUpdateAge);

			$scope.ConnectState = 'Connected';
		}, function () {
			$scope.ConnectState = 'Disconnected';
		});
	}

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
	var interval = $interval(refresh, 5000);
	$scope.$on('$destroy', function () {
		$interval.cancel(interval);
	});
}
