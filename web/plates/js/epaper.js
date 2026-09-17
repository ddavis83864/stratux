angular.module('appControllers').controller('EpaperCtrl', EpaperCtrl);
EpaperCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval'];

// EpaperCtrl drives the optional Waveshare e-paper display page: its
// bounded settings (enable/disable, page, rotation, refresh cadence) via
// the existing generic /getSettings and /setSettings endpoints, and its
// self-reported health via the existing /getHealth endpoint's Epaper
// field (see readiness.EpaperHealth, main/epaperreadiness.go). This page
// never talks to epaper_main directly - it only ever reads/writes
// settings and reads health, exactly like every other optional-feature
// page in this app.
function EpaperCtrl($rootScope, $scope, $state, $http, $interval) {

	$scope.Settings = null;
	$scope.SettingsError = '';
	$scope.Health = null;
	$scope.HealthError = '';
	$scope.Message = '';

	$scope.rotations = [0, 90, 180, 270];
	$scope.pages = [
		{value: 'overview', label: 'Overview - version, readiness, GPS/time trust, storage/overlay'},
		{value: 'receivers', label: 'Receivers - 978/1090/GDL90/traffic count'},
		{value: 'health', label: 'Health - AHRS/baro/fan/power/temperature'}
	];

	$scope.stateClass = function (state) {
		switch (state) {
			case 'READY':
				return 'label-success';
			case 'DEGRADED':
				return 'label-warning';
			case 'NOT_READY':
				return 'label-danger';
			case 'NOT_INSTALLED':
			case 'UNKNOWN':
			default:
				return 'label-default';
		}
	};

	function refreshSettings() {
		$http.get(URL_SETTINGS_GET).then(function (response) {
			var s = response.data;
			$scope.Settings = {
				EpaperEnabled: !!s.EpaperEnabled,
				EpaperPanel: s.EpaperPanel || 'waveshare-3.7in',
				EpaperRotation: s.EpaperRotation || 0,
				EpaperRefreshIntervalSeconds: s.EpaperRefreshIntervalSeconds || 15,
				EpaperFullRefreshEvery: s.EpaperFullRefreshEvery || 20,
				EpaperPage: s.EpaperPage || 'overview'
			};
			$scope.SettingsError = '';
		}, function () {
			$scope.SettingsError = 'Could not reach the settings API.';
		});
	}

	function refreshHealth() {
		$http.get(URL_HEALTH_GET).then(function (response) {
			$scope.Health = response.data.Epaper;
			$scope.HealthError = '';
		}, function () {
			$scope.HealthError = 'Could not reach the health API.';
		});
	}

	// saveSettings sends only the six Epaper* keys this page owns - a
	// partial patch, per /setSettings's own documented contract - never
	// the full settings document, so this page cannot clobber an
	// unrelated setting it never displayed.
	$scope.saveSettings = function () {
		var msg = {
			EpaperEnabled: !!$scope.Settings.EpaperEnabled,
			EpaperPanel: $scope.Settings.EpaperPanel,
			EpaperRotation: parseInt($scope.Settings.EpaperRotation),
			EpaperRefreshIntervalSeconds: parseInt($scope.Settings.EpaperRefreshIntervalSeconds),
			EpaperFullRefreshEvery: parseInt($scope.Settings.EpaperFullRefreshEvery),
			EpaperPage: $scope.Settings.EpaperPage
		};
		$http.post(URL_SETTINGS_SET, msg).then(function () {
			$scope.Message = 'Settings saved.';
			refreshSettings();
		}, function (response) {
			var err = (response.data && response.data.error) ? response.data.error : 'unknown error';
			$scope.Message = 'Failed to save settings: ' + err;
		});
	};

	refreshSettings();
	refreshHealth();
	var interval = $interval(refreshHealth, 5000);
	$scope.$on('$destroy', function () {
		$interval.cancel(interval);
	});
}
