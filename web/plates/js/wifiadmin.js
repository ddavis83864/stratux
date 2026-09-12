/*
wifiadmin.js: dashboard controller for the Wi-Fi administration-hardening
feature (main/wifiadminapi.go, the wifiadmin package). Implements the
preview -> apply -> reconnect -> confirm workflow; never shows a
passphrase back from the server (only *Set booleans), and never claims
the network is "secure" merely because a password is configured - see
docs/wifi-administration-hardening.md.
*/
appControllers.controller('WifiAdminCtrl', function ($scope, $http, $interval) {
	$scope.status = null;
	$scope.statusError = '';

	// form holds the proposed configuration an owner is editing - seeded
	// from the current last-known-good (redacted) on load, never
	// pre-filled with a real passphrase (blank means "leave unchanged"
	// only in the sense that a blank + securityEnabled=true PREVIEW will
	// be rejected server-side, exactly as it should be: this feature
	// never guesses a still-secret value on an owner's behalf).
	$scope.form = {
		ssid: '',
		securityEnabled: false,
		passphrase: '',
		channel: 1,
		country: '',
		mode: 0,
		ipAddress: '192.168.10.1',
		internetPassThroughEnabled: false
	};

	$scope.preview = null;
	$scope.applyToken = null;
	$scope.reconnectToken = null;
	$scope.previewError = '';
	$scope.applyError = '';
	$scope.confirmError = '';
	$scope.confirming = false;
	$scope.applying = false;
	$scope.previewing = false;

	function refreshStatus() {
		$http.get(URL_WIFIADMIN_STATUS).then(function (response) {
			$scope.status = response.data;
			$scope.statusError = '';
			if ($scope.status.lastKnownGood && !$scope.formSeeded) {
				var g = $scope.status.lastKnownGood;
				$scope.form.ssid = g.ssid || '';
				$scope.form.securityEnabled = !!g.securityEnabled;
				$scope.form.channel = g.channel || 1;
				$scope.form.country = g.country || '';
				$scope.form.mode = g.mode || 0;
				$scope.form.ipAddress = g.ipAddress || '192.168.10.1';
				$scope.form.internetPassThroughEnabled = !!g.internetPassThroughEnabled;
				$scope.formSeeded = true;
			}
		}, function (err) {
			$scope.statusError = 'Could not load Wi-Fi administration status.';
		});
	}

	refreshStatus();
	var statusPoll = $interval(refreshStatus, 5000);
	$scope.$on('$destroy', function () {
		if (statusPoll) { $interval.cancel(statusPoll); }
	});

	$scope.previewChange = function () {
		$scope.previewError = '';
		$scope.preview = null;
		$scope.applyToken = null;
		$scope.previewing = true;
		var body = {
			schemaVersion: 1,
			ssid: $scope.form.ssid,
			securityEnabled: $scope.form.securityEnabled,
			passphrase: $scope.form.securityEnabled ? $scope.form.passphrase : '',
			channel: parseInt($scope.form.channel, 10),
			country: $scope.form.country,
			mode: parseInt($scope.form.mode, 10),
			ipAddress: $scope.form.ipAddress,
			internetPassThroughEnabled: $scope.form.internetPassThroughEnabled
		};
		$http.post(URL_WIFIADMIN_PREVIEW, body).then(function (response) {
			$scope.previewing = false;
			$scope.preview = response.data.preview;
			$scope.applyToken = response.data.applyToken;
		}, function (err) {
			$scope.previewing = false;
			$scope.previewError = (err.data && err.data.error) || 'Preview failed.';
		});
	};

	$scope.applyChange = function () {
		if (!$scope.applyToken) { return; }
		$scope.applyError = '';
		$scope.applying = true;
		$http.post(URL_WIFIADMIN_APPLY, { applyToken: $scope.applyToken }).then(function (response) {
			$scope.applying = false;
			$scope.applyToken = null;
			$scope.preview = null;
			// Kept in memory for the common case where this page's own
			// connection survives the change (e.g. only the channel
			// changed) and the owner can confirm without reloading. If
			// the browser DOES lose its connection and reload, this
			// value is gone - the owner then relies on the automatic
			// rollback deadline instead; see this file's own doc
			// comment on confirmReconnection.
			$scope.reconnectToken = response.data.reconnectToken;
			refreshStatus();
		}, function (err) {
			$scope.applying = false;
			$scope.applyError = (err.data && err.data.error) || 'Apply failed.';
			refreshStatus();
		});
	};

	// confirmReconnection is called by the owner once they have
	// reconnected to the (possibly renamed/re-secured) network and
	// reloaded this page - the server's own reconnectTokenAvailable flag
	// on /getWifiAdminStatus tells the client a token exists, but the
	// token VALUE itself is never broadcast in status polling; it is
	// only ever returned once, in the /applyWifiAdminSettings response,
	// and here we simply re-request status and let the owner press
	// Confirm, which the server accepts precisely because reaching this
	// page again over HTTP already proves reachability. If the owner's
	// browser session lost the token (e.g. reloaded the page after
	// applying), this page cannot re-derive it - see the "reconnect
	// token not available" hint in the template; automatic rollback
	// after the deadline is the safety net for exactly that case.
	$scope.confirmReconnection = function (token) {
		if (!token) { return; }
		$scope.confirmError = '';
		$scope.confirming = true;
		$http.post(URL_WIFIADMIN_CONFIRM, { reconnectToken: token }).then(function (response) {
			$scope.confirming = false;
			refreshStatus();
		}, function (err) {
			$scope.confirming = false;
			$scope.confirmError = (err.data && err.data.error) || 'Confirmation failed.';
		});
	};

	$scope.cancelChange = function () {
		$http.post(URL_WIFIADMIN_CANCEL, {}).then(function () {
			$scope.preview = null;
			$scope.applyToken = null;
			refreshStatus();
		});
	};

	$scope.requestRollback = function () {
		$http.post(URL_WIFIADMIN_ROLLBACK, {}).then(function () {
			refreshStatus();
		}, function (err) {
			$scope.statusError = (err.data && err.data.error) || 'Rollback request failed.';
		});
	};
});
