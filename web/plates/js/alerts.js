appControllers.controller('AlertsCtrl', function ($scope, $http, $interval, alertAudioService) {
	$scope.Alerts = { active: [], recentHistory: [], counters: {}, muted: false, trackedTargetCount: 0, disclaimer: '' };
	$scope.Settings = null;
	$scope.AudioState = 'unavailable'; // 'unavailable' | 'armed' | 'suspended'
	$scope.Message = '';
	$scope.MuteMinutes = 30;

	var busy = false;
	var lastSeq = 0;

	function refreshAudioState() {
		if (!alertAudioService.isArmed()) { $scope.AudioState = 'unavailable'; return; }
		var st = alertAudioService.contextState();
		$scope.AudioState = (st === 'running') ? 'armed' : (st === 'suspended' ? 'suspended' : 'unavailable');
	}

	// refresh polls current alerts. An overlap guard (busy) prevents a slow
	// response from stacking concurrent requests - matches the existing
	// Recording/Preflight panels' own polling pattern.
	function refresh() {
		if (busy) return;
		busy = true;
		$http.get(URL_ALERTS_GET).then(function (response) {
			busy = false;
			var snap = response.data;
			$scope.Alerts = snap;
			(snap.recentHistory || []).forEach(function (ev) {
				if (ev.seq > lastSeq) {
					lastSeq = ev.seq;
					if (ev.audioEligible) {
						alertAudioService.playPattern(ev.level, $scope.Settings ? $scope.Settings.audioVolume : 0.5);
					}
				}
			});
			refreshAudioState();
		}, function () {
			busy = false;
		});
	}

	function refreshSettings() {
		$http.get(URL_ALERT_SETTINGS_GET).then(function (response) {
			$scope.Settings = response.data;
		});
	}

	// enableSound is the one place an AudioContext may be created - only
	// ever from this direct button click (a real user gesture), never
	// automatically. See docs/alerting.md's browser-audio section.
	$scope.enableSound = function () {
		var ok = alertAudioService.arm();
		refreshAudioState();
		if (ok && $scope.Settings) {
			$scope.Settings.browserAudioEnabled = true;
			$scope.saveSettings();
		}
	};

	// testSound plays a tone without creating a real alert event - the
	// server call only confirms the subsystem is reachable.
	$scope.testSound = function () {
		$http.post(URL_ALERTS_TEST_SOUND, {}).then(function () {
			alertAudioService.playPattern('TRAFFIC_NOTICE', $scope.Settings ? $scope.Settings.audioVolume : 0.5);
		});
	};

	$scope.mute = function () {
		var seconds = ($scope.MuteMinutes > 0) ? $scope.MuteMinutes * 60 : 0;
		$http.post(URL_ALERTS_MUTE, { durationSeconds: seconds }).then(function () {
			$scope.Message = 'Muted.';
			refresh();
		}, function () {
			$scope.Message = 'Mute request failed.';
		});
	};

	$scope.unmute = function () {
		$http.post(URL_ALERTS_UNMUTE, {}).then(function () {
			$scope.Message = 'Unmuted.';
			refresh();
		}, function () {
			$scope.Message = 'Unmute request failed.';
		});
	};

	$scope.acknowledge = function (id) {
		$http.post(URL_ALERT_ACKNOWLEDGE + '?id=' + encodeURIComponent(id), {}).then(function () {
			refresh();
		});
	};

	$scope.saveSettings = function () {
		$http.post(URL_ALERT_SETTINGS_SET, $scope.Settings).then(function (response) {
			$scope.Settings = response.data.settings;
			$scope.Message = 'Settings saved.';
		}, function (response) {
			var err = (response.data && response.data.error) ? response.data.error : 'unknown error';
			$scope.Message = 'Failed to save settings: ' + err;
		});
	};

	refreshSettings();
	refresh();
	var interval = $interval(refresh, 3000);
	$scope.$on('$destroy', function () {
		$interval.cancel(interval);
	});
});
