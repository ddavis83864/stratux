angular.module('appControllers').controller('FISBCacheCtrl', FISBCacheCtrl); // get the main module contollers set
FISBCacheCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval']; // Inject my dependencies

// create our controller function with all necessary logic
function FISBCacheCtrl($rootScope, $scope, $state, $http, $interval) {

	$scope.Status = undefined;
	$scope.StatusError = '';
	$scope.Settings = undefined;
	$scope.Inventory = [];
	$scope.Message = '';
	$scope.PurgeToken = '';
	$scope.PurgeMessage = '';

	$scope.stateClass = function (state) {
		switch (state) {
			case 'LIVE':
				return 'label-success';
			case 'STARTUP_GRACE':
			case 'WAITING_FOR_TRUSTED_TIME':
			case 'DEGRADED':
				return 'label-warning';
			case 'PRESSURE_INHIBITED':
			case 'READ_ONLY':
			case 'ERROR':
				return 'label-danger';
			default: // DISABLED
				return 'label-default';
		}
	};

	$scope.freshnessClass = function (freshness) {
		switch (freshness) {
			case 'LIVE':
			case 'CACHED_FRESH':
				return 'label-success';
			case 'CACHED_AGING':
				return 'label-info';
			case 'STALE':
				return 'label-warning';
			case 'EXPIRED':
			case 'INVALID':
				return 'label-danger';
			default: // UNSUPPORTED
				return 'label-default';
		}
	};

	$scope.refresh = function () {
		$http.get(URL_FISBCACHE_STATUS_GET).
			then(function (response) {
				$scope.Status = response.data;
				$scope.StatusError = '';
			}, function (errorResponse) {
				$scope.StatusError = 'Failed to load weather cache status.';
			});
		$http.get(URL_FISBCACHE_INVENTORY_GET).
			then(function (response) {
				$scope.Inventory = response.data || [];
			}, function (errorResponse) {
				// leave any previously-loaded inventory in place
			});
	};

	$scope.refreshSettings = function () {
		$http.get(URL_FISBCACHE_SETTINGS_GET).
			then(function (response) {
				$scope.Settings = response.data;
			}, function (errorResponse) {
				// leave any previously-loaded settings in place
			});
	};

	$scope.saveSettings = function () {
		$scope.Message = '';
		$http.post(URL_FISBCACHE_SETTINGS_SET, $scope.Settings).
			then(function (response) {
				$scope.Settings = response.data.settings;
				$scope.Message = 'Settings saved.';
			}, function (errorResponse) {
				var err = (errorResponse.data && errorResponse.data.error) ? errorResponse.data.error : 'unknown error';
				$scope.Message = 'Failed to save settings: ' + err;
			});
	};

	$scope.preparePurge = function () {
		$scope.PurgeMessage = '';
		$http.post(URL_FISBCACHE_PURGE_PREPARE, {}).
			then(function (response) {
				$scope.PurgeToken = response.data.token;
			}, function (errorResponse) {
				$scope.PurgeMessage = 'Failed to prepare purge.';
			});
	};

	$scope.cancelPurge = function () {
		var token = $scope.PurgeToken;
		$scope.PurgeToken = '';
		if (!token) {
			return;
		}
		$http.post(URL_FISBCACHE_PURGE_CANCEL, {}); // best-effort; UI has already dropped the token either way
	};

	$scope.confirmPurge = function () {
		$http.post(URL_FISBCACHE_PURGE_CONFIRM, {token: $scope.PurgeToken}).
			then(function (response) {
				$scope.PurgeToken = '';
				$scope.PurgeMessage = 'Cache purged (' + response.data.deletedCount + ' entries removed).';
				$scope.refresh();
			}, function (errorResponse) {
				$scope.PurgeToken = '';
				var err = (errorResponse.data && errorResponse.data.error) ? errorResponse.data.error : 'unknown error';
				$scope.PurgeMessage = 'Failed to purge cache: ' + err;
			});
	};

	$scope.refresh();
	$scope.refreshSettings();

	var fisbCacheRefreshInterval = $interval(function () {
		$scope.refresh();
	}, 3000);

	$scope.$on('$destroy', function () {
		if (fisbCacheRefreshInterval) {
			$interval.cancel(fisbCacheRefreshInterval);
			fisbCacheRefreshInterval = undefined;
		}
	});
}
