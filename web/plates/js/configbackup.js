appControllers.controller('ConfigBackupCtrl', function ($scope, $http) {
	$scope.downloadUrl = URL_CONFIGBACKUP_DOWNLOAD;
	// includePrivacy is the explicit, off-by-default opt-in for ownship/
	// owner-identifying fields - the ordinary Download button must
	// produce a sanitized backup unless the owner deliberately checks
	// this first. See docs/configuration-backup-restore.md.
	$scope.includePrivacy = false;

	$scope.selectedFileName = '';
	$scope.selectedFileText = '';
	$scope.validating = false;
	$scope.validateError = '';
	$scope.validateErrors = [];
	$scope.preview = null;
	$scope.confirmationToken = null;
	$scope.confirmApply = false;
	$scope.applying = false;
	$scope.applied = false;
	$scope.applyResult = null;

	// resetForNewFile clears every result tied to a previously-selected
	// or previously-validated file - a new file selection always starts
	// a fresh validate/preview/confirm cycle, never carries forward a
	// stale preview or confirmation token for different content.
	function resetForNewFile() {
		$scope.validateError = '';
		$scope.validateErrors = [];
		$scope.preview = null;
		$scope.confirmationToken = null;
		$scope.confirmApply = false;
		$scope.applying = false;
		$scope.applied = false;
		$scope.applyResult = null;
	}

	$scope.onBackupFileSelected = function (files) {
		resetForNewFile();
		var file = files && files[0];
		if (!file) {
			$scope.selectedFileName = '';
			$scope.selectedFileText = '';
			$scope.$apply();
			return;
		}
		$scope.selectedFileName = file.name;
		var reader = new FileReader();
		reader.onload = function (e) {
			$scope.selectedFileText = e.target.result;
			$scope.$apply();
		};
		reader.onerror = function () {
			$scope.validateError = 'Could not read the selected file.';
			$scope.$apply();
		};
		reader.readAsText(file);
	};

	$scope.validateBackup = function () {
		if (!$scope.selectedFileText) {
			return;
		}
		resetForNewFile();
		$scope.validating = true;
		$http.post(URL_CONFIGBACKUP_VALIDATE, $scope.selectedFileText, {
			headers: {'Content-Type': 'application/json'}
		}).then(function (response) {
			$scope.validating = false;
			$scope.preview = response.data.preview;
			$scope.confirmationToken = response.data.confirmationToken;
		}, function (response) {
			$scope.validating = false;
			var data = response.data || {};
			$scope.validateError = data.error || 'Validation failed.';
			$scope.validateErrors = data.errors || [];
		});
	};

	$scope.applyBackup = function () {
		if (!$scope.confirmationToken || !$scope.confirmApply || $scope.applying || $scope.applied) {
			return;
		}
		$scope.applying = true;
		var backup;
		try {
			backup = JSON.parse($scope.selectedFileText);
		} catch (e) {
			$scope.applying = false;
			$scope.validateError = 'The selected file is no longer valid JSON.';
			return;
		}
		$http.post(URL_CONFIGBACKUP_APPLY, {
			confirmationToken: $scope.confirmationToken,
			backup: backup
		}).then(function (response) {
			$scope.applying = false;
			$scope.applied = true;
			$scope.applyResult = response.data.result;
			$scope.applyResult.success = true;
		}, function (response) {
			$scope.applying = false;
			$scope.applied = true;
			var data = response.data || {};
			$scope.applyResult = data.result || {};
			$scope.applyResult.success = false;
			$scope.applyResult.failureCategory = data.error || 'Restore failed.';
		});
	};
});
