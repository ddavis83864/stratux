angular.module('appControllers').controller('StorageCtrl', StorageCtrl);
StorageCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval'];

// StorageCtrl drives the Storage page: a read-only view of the storage-
// lifecycle inventory foundation (see main/storagelifecycleapi.go and
// docs/storage-lifecycle.md). This page never deletes anything - there is
// no delete control anywhere on it, and no such API exists yet.
function StorageCtrl($rootScope, $scope, $state, $http, $interval) {

	$scope.Storage = null;
	$scope.StorageError = '';

	$scope.pressureClass = function (pressure) {
		switch (pressure) {
			case 'CRITICAL': return 'label-danger';
			case 'HIGH': return 'label-warning';
			case 'ELEVATED': return 'label-warning';
			case 'NORMAL': return 'label-success';
			default: return 'label-default';
		}
	};

	// namespaceIds returns Storage.namespaces' keys sorted, so the table
	// renders in a stable order across polls instead of reshuffling with
	// whatever order Angular's object iteration happens to produce.
	$scope.namespaceIds = function () {
		if (!$scope.Storage || !$scope.Storage.namespaces) return [];
		return Object.keys($scope.Storage.namespaces).sort();
	};

	function refresh() {
		$http.get(URL_STORAGE_LIFECYCLE_GET).then(function (response) {
			$scope.Storage = response.data;
			$scope.StorageError = '';
		}, function () {
			$scope.StorageError = 'Could not reach the storage-lifecycle API.';
		});
	}

	refresh();
	var interval = $interval(refresh, 10000);
	$scope.$on('$destroy', function () {
		$interval.cancel(interval);
	});
}
