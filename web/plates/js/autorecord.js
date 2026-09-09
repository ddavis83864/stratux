angular.module('appControllers').controller('AutoRecordCtrl', AutoRecordCtrl);
AutoRecordCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval'];

// AutoRecordCtrl drives the Auto Record page: status of the Automatic
// Flight Recording feature (see main/autorecordapi.go and
// docs/automatic-flight-recording.md) plus its bounded settings controls.
// This page never starts or stops a recording directly - only the
// enable/disable toggle and threshold settings; starting/stopping is
// always the Machine's own decision (or the existing manual controls on
// the Settings/recording page).
function AutoRecordCtrl($rootScope, $scope, $state, $http, $interval) {

	$scope.Status = null;
	$scope.StatusError = '';
	$scope.Settings = null;
	$scope.Message = '';

	$scope.stateClass = function (state) {
		switch (state) {
			case 'RECORDING':
			case 'ARMED_WAITING':
			case 'START_CANDIDATE':
			case 'STOP_CANDIDATE':
			case 'STARTING':
			case 'FINALIZING':
				return 'label-success';
			case 'INHIBITED':
				return 'label-warning';
			case 'ERROR':
				return 'label-danger';
			default: // DISABLED
				return 'label-default';
		}
	};

	function refresh() {
		$http.get(URL_AUTORECORD_STATUS_GET).then(function (response) {
			$scope.Status = response.data;
			$scope.StatusError = '';
		}, function () {
			$scope.StatusError = 'Could not reach the automatic-recording API.';
		});
	}

	function refreshSettings() {
		$http.get(URL_AUTORECORD_SETTINGS_GET).then(function (response) {
			$scope.Settings = response.data;
		});
	}

	$scope.saveSettings = function () {
		$http.post(URL_AUTORECORD_SETTINGS_SET, $scope.Settings).then(function (response) {
			$scope.Settings = response.data.settings;
			$scope.Message = 'Settings saved.';
		}, function (response) {
			var err = (response.data && response.data.error) ? response.data.error : 'unknown error';
			$scope.Message = 'Failed to save settings: ' + err;
		});
	};

	$scope.clearError = function () {
		$http.post(URL_AUTORECORD_CLEAR_ERROR, {}).then(function () {
			refresh();
		}, function (response) {
			var err = (response.data && response.data.error) ? response.data.error : 'unknown error';
			$scope.Message = 'Failed to clear error: ' + err;
		});
	};

	refreshSettings();
	refresh();
	var interval = $interval(refresh, 3000);
	$scope.$on('$destroy', function () {
		$interval.cancel(interval);
	});
}
