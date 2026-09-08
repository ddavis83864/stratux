// application constants
var URL_HOST_BASE           = window.location.hostname + (window.location.port ? ':' + window.location.port : '');
var URL_HOST_PROTOCOL       = window.location.protocol + "//";

var URL_AHRS_CAGE           = URL_HOST_PROTOCOL + URL_HOST_BASE + "/cageAHRS";
var URL_AHRS_CAL            = URL_HOST_PROTOCOL + URL_HOST_BASE + "/calibrateAHRS";
var URL_AHRS_ORIENT         = URL_HOST_PROTOCOL + URL_HOST_BASE + "/orientAHRS";
var URL_DELETEAHRSLOGFILES  = URL_HOST_PROTOCOL + URL_HOST_BASE + "/deleteahrslogfiles";
var URL_DELETELOGFILE       = URL_HOST_PROTOCOL + URL_HOST_BASE + "/deletelogfile";
var URL_DEV_TOGGLE_GET      = URL_HOST_PROTOCOL + URL_HOST_BASE + "/develmodetoggle";
var URL_DOWNLOADAHRSLOGFILES = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloadahrslogs";
var URL_DOWNLOADDB          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloaddb";
var URL_DOWNLOADLOGFILE     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloadlog";
var URL_GMETER_RESET        = URL_HOST_PROTOCOL + URL_HOST_BASE + "/resetGMeter";
var URL_REBOOT              = URL_HOST_PROTOCOL + URL_HOST_BASE + "/reboot";
var URL_RESTARTAPP          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/restart";
var URL_SATELLITES_GET      = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getSatellites";
var URL_SETTINGS_GET        = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getSettings";
var URL_SETTINGS_SET        = URL_HOST_PROTOCOL + URL_HOST_BASE + "/setSettings";
var URL_SHUTDOWN            = URL_HOST_PROTOCOL + URL_HOST_BASE + "/shutdown";
var URL_STATUS_GET          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getStatus";
var URL_HEALTH_GET          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getHealth";
var URL_REGION_GET          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getRegion";
var URL_REGION_SET          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/setRegion";
var URL_TOWERS_GET          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getTowers";
var URL_UPDATE_UPLOAD       = URL_HOST_PROTOCOL + URL_HOST_BASE + "/updateUpload";
var URL_UPDATE_PONG         = URL_HOST_PROTOCOL + URL_HOST_BASE + "/updatePong";
var URL_GET_SITUATION       = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getSituation";
var URL_GET_TILESETS        = URL_HOST_PROTOCOL + URL_HOST_BASE + "/tiles/tilesets";
var URL_GET_TILE            = URL_HOST_PROTOCOL + URL_HOST_BASE + "/tiles";
var URL_GET_STYLE           = URL_HOST_PROTOCOL + URL_HOST_BASE + "/mapdata/styles"

var URL_DIAGNOSTICS_GENERATE = URL_HOST_PROTOCOL + URL_HOST_BASE + "/generateDiagnostics";
var URL_DIAGNOSTICS_LIST     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getDiagnostics";
var URL_DIAGNOSTICS_DOWNLOAD = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloadDiagnostics";
var URL_RECORDING_START      = URL_HOST_PROTOCOL + URL_HOST_BASE + "/startRecording";
var URL_RECORDING_STOP       = URL_HOST_PROTOCOL + URL_HOST_BASE + "/stopRecording";
var URL_RECORDING_STATUS     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getRecordingStatus";
var URL_RECORDING_LIST       = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getRecordings";
var URL_RECORDING_EXPORT     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/exportRecording";
var URL_RECORDING_DOWNLOAD   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloadRecording";
var URL_EXPORT_DOWNLOAD      = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloadExport";
var URL_RECORDING_METADATA          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getRecordingMetadata";
var URL_RECORDING_METADATA_DOWNLOAD = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloadRecordingMetadata";
var URL_ALERTS_GET           = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getAlerts";
var URL_ALERT_SETTINGS_GET   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getAlertSettings";
var URL_ALERT_SETTINGS_SET   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/setAlertSettings";
var URL_ALERT_ACKNOWLEDGE    = URL_HOST_PROTOCOL + URL_HOST_BASE + "/acknowledgeAlert";
var URL_ALERTS_MUTE          = URL_HOST_PROTOCOL + URL_HOST_BASE + "/muteAlerts";
var URL_ALERTS_UNMUTE        = URL_HOST_PROTOCOL + URL_HOST_BASE + "/unmuteAlerts";
var URL_ALERTS_TEST_SOUND    = URL_HOST_PROTOCOL + URL_HOST_BASE + "/testAlertSound";
var URL_CONFIGBACKUP_DOWNLOAD = URL_HOST_PROTOCOL + URL_HOST_BASE + "/downloadConfigurationBackup";
var URL_CONFIGBACKUP_VALIDATE = URL_HOST_PROTOCOL + URL_HOST_BASE + "/validateConfigurationBackup";
var URL_CONFIGBACKUP_APPLY    = URL_HOST_PROTOCOL + URL_HOST_BASE + "/applyConfigurationBackup";
var URL_CONFIGBACKUP_STATUS   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getConfigurationRestoreStatus";
var URL_CALPROFILES_LIST     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getCalibrationProfiles";
var URL_CALPROFILES_ACTIVE   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getActiveCalibrationProfile";
var URL_CALPROFILES_CREATE   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/createCalibrationProfile";
var URL_CALPROFILES_UPDATE   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/updateCalibrationProfile";
var URL_CALPROFILES_ACTIVATE = URL_HOST_PROTOCOL + URL_HOST_BASE + "/activateCalibrationProfile";
var URL_CALPROFILES_DELETE   = URL_HOST_PROTOCOL + URL_HOST_BASE + "/deleteCalibrationProfile";

var URL_PREFLIGHT_GET        = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getPreflightReport";
var URL_PREFLIGHT_ACK        = URL_HOST_PROTOCOL + URL_HOST_BASE + "/acknowledgePreflightCheck";
var URL_PREFLIGHT_CLEAR      = URL_HOST_PROTOCOL + URL_HOST_BASE + "/clearPreflightCheck";
var URL_PREFLIGHT_RESET      = URL_HOST_PROTOCOL + URL_HOST_BASE + "/resetPreflightChecks";

var URL_POWER_HEALTH_GET     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getPowerHealth";
var URL_SHUTDOWN_STATUS_GET  = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getShutdownStatus";
var URL_SHUTDOWN_REQUEST     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/requestShutdown";
var URL_SHUTDOWN_CONFIRM     = URL_HOST_PROTOCOL + URL_HOST_BASE + "/confirmShutdown";

var URL_STORAGE_LIFECYCLE_GET = URL_HOST_PROTOCOL + URL_HOST_BASE + "/getStorageLifecycle";


var URL_DEVELOPER_WS        = "ws://" + URL_HOST_BASE + "/developer";
var URL_GPS_WS              = "ws://" + URL_HOST_BASE + "/situation";
var URL_STATUS_WS           = "ws://" + URL_HOST_BASE + "/status";
var URL_TRAFFIC_WS          = "ws://" + URL_HOST_BASE + "/traffic";
var URL_WEATHER_WS          = "ws://" + URL_HOST_BASE + "/weather";
var URL_RADAR_WS            = "ws://" + URL_HOST_BASE + "/radar";

// define the module with dependency on mobile-angular-ui
//var app = angular.module('stratux', ['ngRoute', 'mobile-angular-ui', 'mobile-angular-ui.gestures', 'appControllers']);
var app = angular.module('stratux', ['ui.router', 'mobile-angular-ui', 'mobile-angular-ui.gestures', 'appControllers']);
var appControllers = angular.module('appControllers', []);
//cutoff value to remove targets out of the list, keep in sync with the value in traffic.go for cleanUpOldEntries, keep it just below cutoff value in traffic.go
let TRAFFIC_MAX_AGE_SECONDS = 59;
let TRAFFIC_AIS_MAX_AGE_SECONDS = 60*15;
let TARGET_TYPE_AIS = 5;

app.config(function ($stateProvider, $urlRouterProvider) {
	$stateProvider
		.state('home', {
			url: '/',
			templateUrl: 'plates/status.html',
			controller: 'StatusCtrl',
			reloadOnSearch: false
		})
		.state('readiness', {
			url: '/readiness',
			templateUrl: 'plates/readiness.html',
			controller: 'ReadinessCtrl',
			reloadOnSearch: false
		})
		.state('preflight', {
			url: '/preflight',
			templateUrl: 'plates/preflight.html',
			controller: 'PreflightCtrl',
			reloadOnSearch: false
		})
		.state('alerts', {
			url: '/alerts',
			templateUrl: 'plates/alerts.html',
			controller: 'AlertsCtrl',
			reloadOnSearch: false
		})
		.state('configbackup', {
			url: '/configbackup',
			templateUrl: 'plates/configbackup.html',
			controller: 'ConfigBackupCtrl',
			reloadOnSearch: false
		})
		.state('power', {
			url: '/power',
			templateUrl: 'plates/power.html',
			controller: 'PowerCtrl',
			reloadOnSearch: false
		})
		.state('storage', {
			url: '/storage',
			templateUrl: 'plates/storage.html',
			controller: 'StorageCtrl',
			reloadOnSearch: false
		})
		.state('towers', {
			url: '/towers',
			templateUrl: 'plates/towers.html',
			controller: 'TowersCtrl',
			reloadOnSearch: false
		})
		.state('weather', {
			url: '/weather',
			templateUrl: 'plates/weather.html',
			controller: 'WeatherCtrl',
			reloadOnSearch: false
		})
		.state('traffic', {
			url: '/traffic',
			templateUrl: 'plates/traffic.html',
			controller: 'TrafficCtrl',
			reloadOnSearch: false
		})
		.state('gps', {
			url: '/gps',
			templateUrl: 'plates/gps.html',
			controller: 'GPSCtrl',
			reloadOnSearch: false
		})
		.state('logs', {
			url: '/logs',
			templateUrl: 'plates/logs.html',
			controller: 'LogsCtrl',
			reloadOnSearch: false
		})
		.state('settings', {
			url: '/settings',
			templateUrl: 'plates/settings.html',
			controller: 'SettingsCtrl',
			reloadOnSearch: false
		})
		.state('radar', {
			url: '/radar',
			templateUrl: 'plates/radar.html',
			controller: 'RadarCtrl',
			reloadOnSearch: false
		})
		.state('map', {
			url: '/map',
			templateUrl: 'plates/map.html',
			controller: 'MapCtrl',
			reloadOnSearch: false
		})
        .state('developer', {
			url: '/developer',
			templateUrl: 'plates/developer.html',
			controller: 'DeveloperCtrl',
			reloadOnSearch: false
		});
	$urlRouterProvider.otherwise('/');
});


app.run(function ($transform) {
	window.$transform = $transform;
});

// For this app we have a MainController for whatever and individual controllers for each page
app.controller('MainCtrl', function ($scope, $http, $interval, alertAudioService) {
	// any logic global logic
    $http.get(URL_SETTINGS_GET)
    .then(function(response) {
			var settings = angular.fromJson(response.data);
            $scope.DeveloperMode = settings.DeveloperMode;
            $scope.UAT_Enabled = settings.UAT_Enabled;
            $scope.Ping_Enabled = settings.Ping_Enabled;
            $scope.Pong_Enabled = settings.Pong_Enabled;

            // Update theme
            $scope.updateTheme(settings.DarkMode);
    }, function(response) {
        //Second function handles error
    });	

    $scope.updateTheme = function(darkMode) {
        if(darkMode != $scope.DarkMode) {
            // console.log("Updating theme, use dark mode?", darkMode);
            $scope.DarkMode = darkMode;

            if($scope.DarkMode) {
                document.getElementById('themeStylesheet').href = 'css/dark-mode.css';
            } else {
                document.getElementById('themeStylesheet').href = '';
            }
        }
    };

    // --- Global alert indicator -------------------------------------
    // A small, always-visible (every page, via the persistent navbar in
    // index.html) summary of the worst currently-active alert level and
    // mute state - see docs/alerting.md. Also the one place audio is
    // triggered for events discovered outside the dedicated Alerts page,
    // so a tone still plays even while the operator is looking at
    // Traffic/Readiness/etc. Polling here is independent of, and in
    // addition to, AlertsCtrl's own faster poll while that page is open.
    $scope.AlertIndicator = { level: 'none', muted: false };
    var alertIndicatorRank = { 'none': 0, 'TRAFFIC_NOTICE': 1, 'TRAFFIC_CAUTION': 2, 'SYSTEM_CAUTION': 3, 'SYSTEM_NOT_READY': 4 };
    var lastGlobalAlertSeq = 0;
    var globalAlertPollBusy = false;
    var globalAlertVolume = 0.5;
    $http.get(URL_ALERT_SETTINGS_GET).then(function (response) {
        if (response.data && typeof response.data.audioVolume === 'number') {
            globalAlertVolume = response.data.audioVolume;
        }
    });
    function pollGlobalAlertIndicator() {
        if (globalAlertPollBusy) return;
        globalAlertPollBusy = true;
        $http.get(URL_ALERTS_GET).then(function (response) {
            globalAlertPollBusy = false;
            var snap = response.data;
            $scope.AlertIndicator.muted = !!snap.muted;
            var worst = 'none';
            (snap.active || []).forEach(function (a) {
                if ((alertIndicatorRank[a.level] || 0) > (alertIndicatorRank[worst] || 0)) worst = a.level;
            });
            $scope.AlertIndicator.level = worst;
            (snap.recentHistory || []).forEach(function (ev) {
                if (ev.seq > lastGlobalAlertSeq) {
                    lastGlobalAlertSeq = ev.seq;
                    if (ev.audioEligible) alertAudioService.playPattern(ev.level, globalAlertVolume);
                }
            });
        }, function () {
            globalAlertPollBusy = false;
        });
    }
    pollGlobalAlertIndicator();
    var alertIndicatorInterval = $interval(pollGlobalAlertIndicator, 5000);
    $scope.$on('$destroy', function () {
        $interval.cancel(alertIndicatorInterval);
    });
})
.service('alertAudioService', function () {
	// Short, conservative tones for the three audible alert levels -
	// never continuous, never started without an explicit user gesture
	// (arm() is only ever called from a direct "Enable Sound" button
	// click - see web/plates/js/alerts.js). See docs/alerting.md's
	// browser-audio section for the iOS/backgrounding limitations this
	// deliberately does not try to work around.
	var ctx = null;
	var armed = false;

	function ensureContext() {
		if (!ctx) {
			var AudioCtor = window.AudioContext || window.webkitAudioContext;
			if (!AudioCtor) return null;
			ctx = new AudioCtor();
		}
		return ctx;
	}

	this.arm = function () {
		var c = ensureContext();
		if (!c) return false;
		if (c.state === 'suspended' && c.resume) c.resume();
		armed = true;
		return true;
	};

	this.disarm = function () { armed = false; };
	this.isArmed = function () { return armed && ctx !== null; };
	this.contextState = function () { return ctx ? ctx.state : 'unavailable'; };

	var patterns = {
		'TRAFFIC_NOTICE':   { freq: 880,  beeps: 1, dur: 0.12 },
		'TRAFFIC_CAUTION':  { freq: 1046, beeps: 2, dur: 0.10 },
		'SYSTEM_CAUTION':   { freq: 660,  beeps: 3, dur: 0.15 },
		'SYSTEM_NOT_READY': { freq: 523,  beeps: 3, dur: 0.20 }
	};

	// playPattern is a no-op (returns false) unless arm() has already
	// succeeded - mute is enforced by the caller never invoking this for
	// a muted/audio-ineligible event (see Alert.audioEligible), not by
	// this service re-checking mute state itself.
	this.playPattern = function (level, volume) {
		var c = ensureContext();
		if (!c || !armed) return false;
		var vol = (typeof volume === 'number') ? Math.max(0, Math.min(1, volume)) : 0.5;
		var p = patterns[level] || patterns['TRAFFIC_NOTICE'];
		var t = c.currentTime;
		for (var i = 0; i < p.beeps; i++) {
			var osc = c.createOscillator();
			var gain = c.createGain();
			osc.frequency.value = p.freq;
			osc.type = 'sine';
			gain.gain.setValueAtTime(0, t);
			gain.gain.linearRampToValueAtTime(vol * 0.3, t + 0.01);
			gain.gain.linearRampToValueAtTime(0, t + p.dur);
			osc.connect(gain);
			gain.connect(c.destination);
			osc.start(t);
			osc.stop(t + p.dur + 0.02);
			t += p.dur + 0.08;
		}
		return true;
	};
})
.service('craftService',function(){
	let trafficSourceColors = {
		1: 'cornflowerblue', // ES
		2: '#FF8C00',      // UAT
		4: 'green',          // OGN
		5: '#0077be',         // AIS
		6: 'darkkhaki'     // UAT bar color
	}

	const getTrafficSourceColor = (source) => {
		if (trafficSourceColors[source] !== undefined) {
			return trafficSourceColors[source];
		} else {
			return 'gray';
		}
	}

	// THis ensures that the colors used in traffic.js and map.js for the vessels are the same
	let aircraftColors = {

		10: 'cornflowerblue',
		11: 'cornflowerblue',
		12: 'skyblue',
		13: 'skyblue',
		14: 'skyblue',

		20: 'darkorange',
		21: 'darkorange',
		22: 'orange',
		23: 'orange',
		24: 'orange',

		40: 'green',
		41: 'green',
		42: 'greenyellow',
		43: 'greenyellow',
		44: 'greenyellow'
	}

	const getAircraftColor = (aircraft) => {
		let code = aircraft.Last_source.toString()+aircraft.TargetType.toString();			
		if (aircraftColors[code] === undefined) {
			return 'white';
		} else {
			return aircraftColors[code];
		}
	};

	const getVesselColor = (vessel) => {
		// https://www.navcen.uscg.gov/?pageName=AISMessagesAStatic
		firstDigit = Math.floor(vessel.SurfaceVehicleType / 10)
		secondDigit = vessel.SurfaceVehicleType - Math.floor(vessel.SurfaceVehicleType / 10)*10;

		const categoryFirst= {
			2: 'orange',
			4: 'orange',
			5: 'orange',
			6: 'blue',
			7: 'green',
			8: 'red',
			9: 'red'
		};		
		const categorySecond= {
			0: 'silver',
			1: 'cyan',
			2: 'darkblue',
			3: 'LightSkyBlue',
			4: 'LightSkyBlue',
			5: 'darkolivegreen',
			6: 'maroon',
			7: 'purple'
		};		

		if (categoryFirst[firstDigit]) {
			return categoryFirst[firstDigit];
		} else if (firstDigit===3 && categorySecond[secondDigit]) {
			return categorySecond[secondDigit];
		} else {
			return 'gray';			
		}
	};

	const isTrafficAged = (aircraft, targetVar ) => {
		const value = aircraft[targetVar];
		if (aircraft.TargetType === TARGET_TYPE_AIS) {
			return value > TRAFFIC_AIS_MAX_AGE_SECONDS;
		} else { 
			return value > TRAFFIC_MAX_AGE_SECONDS;
		}
	};

	const getVesselCategory = (vessel) => {
		// https://www.navcen.uscg.gov/?pageName=AISMessagesAStatic
		firstDigit = Math.floor(vessel.SurfaceVehicleType / 10)
		secondDigit = vessel.SurfaceVehicleType - Math.floor(vessel.SurfaceVehicleType / 10)*10;

		const categoryFirst= {
			2: 'Cargo',
			4: 'Cargo',
			5: 'Cargo',
			6: 'Passenger',
			7: 'Cargo',
			8: 'Tanker',
			9: 'Cargo',
		};		
		const categorySecond= {
			0: 'Fishing',
			1: 'Tugs',
			2: 'Tugs',
			3: 'Dredging',
			4: 'Diving',
			5: 'Military',
			6: 'Sailing',
			7: 'Pleasure',
		};		

		if (categoryFirst[firstDigit]) {
			return categoryFirst[firstDigit];
		} else if (firstDigit===3 && categorySecond[secondDigit]) {
			return categorySecond[secondDigit];
		} else {
			return '---';			
		}
	};

	const getAircraftCategory = (aircraft) => {
		const category = {
			1: 'Light',
			2: 'Small',
			3: 'Large',
			4: 'VLarge',
			5: 'Heavy',
			6: 'Fight',
			7: 'Helic',
			9: 'Glide',
			10: 'Ballo',
			11: 'Parac',
			12: 'Ultrl',
			14: 'Drone',
			15: 'Space',
			16: 'VLarge',
			17: 'Vehic',
			18: 'Vehic',
			19: 'Obstc'
		};		
		return category[aircraft.Emitter_category]?category[aircraft.Emitter_category]:'---';
	};

	return {
		getCategory: (craft) => {
			if (craft.TargetType === TARGET_TYPE_AIS) {
				return getVesselCategory(craft);
			} else {
				return getAircraftCategory(craft);
			}
		},

		getTrafficSourceColor: (source) => {
			return getTrafficSourceColor(source);
		},

		isTrafficAged: (craft) => {
			return isTrafficAged(craft, 'Age');
		},

		isTrafficAged2: (craft, targetVar) => {
			return isTrafficAged(craft, targetVar);
		},

		getTransportColor: (craft) => {
			if (craft.TargetType === TARGET_TYPE_AIS) {
				return getVesselColor(craft);
			} else {
				return getAircraftColor(craft);
			}
		}
	
	};

});
