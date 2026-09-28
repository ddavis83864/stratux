angular.module('appControllers').controller('WeatherCtrl', WeatherCtrl); // get the main module contollers set
WeatherCtrl.$inject = ['$rootScope', '$scope', '$state', '$http', '$interval']; // Inject my dependencies

/*
WeatherCtrl: the native FIS-B Weather viewer. Combines two independent
data sources, neither of which alone is enough to show a useful cached-
weather page:

  - /weather (WebSocket): this codebase's original, live-only text-report
    feed (main/gen_gdl90.go's WeatherMessage) - works whether or not the
    rolling FIS-B cache is enabled, but has no history/replay buffer at
    all (see main/managementinterface.go's handleWeatherWS - a report
    that arrived before this page connected is invisible until it
    repeats).
  - /getFISBCacheStatus + /getFISBCacheInventory (polled): the rolling
    cache's own metadata-only view (identity/freshness/age/size, never
    raw content - see main/fisbcacheapi.go's own doc comments) of
    whatever it has cached, whether or not this page was open when it
    arrived. Only populated when the cache feature is enabled
    (Weather Cache page) and only useful for content when this page also
    fetches a specific entry's raw content on demand via
    /getFISBCachePayload (see main/fisbcachepayload.go) - itself only
    possible when the cache's OWN persistence setting is on.

WeatherLogic (weatherlogic.js) owns every pure decision (categorization,
merging, freshness labels, filtering/sorting, NEXRAD decoding, bounded
lists) - this controller only owns HTTP/WebSocket/DOM/interval wiring and
$scope shape.
*/
function WeatherCtrl($rootScope, $scope, $state, $http, $interval) {
	var WL = WeatherLogic;
	var MAX_LIVE_TICKER = 25; // bounded, per-category-independent "just arrived" ticker

	$scope.$parent.helppage = 'plates/weather-help.html';

	$scope.DISCLAIMER = 'FIS-B data shown here is received over ADS-B. Verify data age and use approved flight-planning/weather sources as appropriate.';

	$scope.CacheStatus = null;
	$scope.CacheEnabled = false;
	$scope.CachePersistenceEnabled = false;
	$scope.Rows = [];
	$scope.LiveTicker = []; // bounded list of recently-arrived identities, most recent first

	$scope.Towers = 0;
	$scope.ReceiverFreshnessLabel = 'NO WX FRAMES YET';
	$scope.LastWeatherFrameAgeSeconds = null;
	var lastWeatherCounterSum = null;
	var lastWeatherCounterAt = null;

	$scope.ActiveCategory = WL.CATEGORY_METAR;
	$scope.Categories = [
		{key: WL.CATEGORY_METAR, label: 'METAR/SPECI'},
		{key: WL.CATEGORY_TAF, label: 'TAF'},
		{key: WL.CATEGORY_PIREP, label: 'PIREP'},
		{key: WL.CATEGORY_WINDS, label: 'Winds/Temps Aloft'},
		{key: WL.CATEGORY_NEXRAD, label: 'NEXRAD'},
		{key: WL.CATEGORY_OTHER_TEXT, label: 'Other'},
		{key: 'unsupported', label: 'NOTAM/SIGMET'}
	];

	$scope.StationFilter = '';
	$scope.SearchQuery = '';

	$scope.setActiveCategory = function (key) {
		$scope.ActiveCategory = key;
	};

	// visibleRows only applies the station/search filters for the
	// categories whose template actually exposes those inputs (every
	// text category) - NEXRAD rows have no station and no known raw text
	// until fetched, so applying a leftover filter value from a
	// previously-viewed tab would otherwise silently hide every NEXRAD
	// row rather than genuinely finding none.
	$scope.visibleRows = function () {
		var rows = WL.rowsInCategory($scope.Rows, $scope.ActiveCategory);
		if ($scope.ActiveCategory !== WL.CATEGORY_NEXRAD) {
			rows = WL.filterByStation(rows, $scope.StationFilter);
			rows = WL.searchRawText(rows, $scope.SearchQuery);
		}
		return WL.sortNewestFirst(rows);
	};

	$scope.rowsInCategoryCount = function (key) {
		return WL.rowsInCategory($scope.Rows, key).length;
	};

	$scope.categoryLabel = function (key) {
		for (var i = 0; i < $scope.Categories.length; i++) {
			if ($scope.Categories[i].key === key) {
				return $scope.Categories[i].label;
			}
		}
		return key;
	};

	$scope.freshnessClass = function (freshness) {
		// Matches web/plates/js/fisbcache.js's own freshnessClass exactly -
		// this page and the Weather Cache diagnostics page must never show
		// two different colors for the same freshness value.
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

	$scope.fmtAge = WL.formatAge;

	// receiverFreshnessClass colors the top-of-page RECEIVER-level label
	// (WX RX RECENT/AGING/STALE/NO WX FRAMES YET) - deliberately a
	// separate mapping from freshnessClass above, since this label is not
	// itself a fisbcache.FreshnessState value, just a similarly-styled
	// badge for a different (receiver, not per-product) concept.
	$scope.receiverFreshnessClass = function (label) {
		switch (label) {
			case 'WX RX RECENT':
				return 'label-success';
			case 'WX RX AGING':
				return 'label-info';
			case 'WX RX STALE':
				return 'label-warning';
			default: // NO WX FRAMES YET
				return 'label-default';
		}
	};

	// --- on-demand raw-content fetch (persisted entries only) ---------------

	$scope.canFetchPayload = function (row) {
		return $scope.CachePersistenceEnabled && row.rawTextStatus !== 'live' &&
			row.rawTextStatus !== 'fetched' && row.rawTextStatus !== 'fetching';
	};

	// previewNexradTile is the NEXRAD "Preview tile" button's handler:
	// re-renders instantly from already-known content (live or
	// previously fetched) rather than silently no-op'ing just because a
	// fetch is no longer needed - only reaches the network for content
	// this session doesn't already have.
	$scope.previewNexradTile = function (row) {
		if (row.rawText != null) {
			$scope.renderNexradPreview(row.identity, row.rawText);
			return;
		}
		$scope.fetchPayload(row);
	};

	$scope.fetchPayload = function (row) {
		if (!$scope.canFetchPayload(row)) {
			return;
		}
		$scope.Rows = WL.markFetching($scope.Rows, row.identity);
		$http.get(URL_FISBCACHE_PAYLOAD_GET, {params: {class: row.productClass, identity: row.identity}}).
			then(function (response) {
				$scope.Rows = WL.applyFetchedPayload($scope.Rows, row.identity, response.data.payload);
				if (row.category === WL.CATEGORY_NEXRAD) {
					$scope.renderNexradPreview(row.identity, response.data.payload);
				}
			}, function (errorResponse) {
				$scope.Rows = WL.markFetchFailed($scope.Rows, row.identity);
			});
	};

	// --- NEXRAD ----------------------------------------------------------------

	$scope.NexradPreview = null; // {identity, bounds, stats, canvasId}

	$scope.nexradBounds = function (identity) {
		return WL.parseNexradIdentity(identity);
	};

	// renderNexradPreview decodes a fetched tile's base64 intensity data
	// and draws a plain per-bin color ramp onto a canvas - metadata (task
	// section 10's minimum) is always shown regardless; this is the
	// explicitly-optional visualization on top of it, never a blocker.
	$scope.renderNexradPreview = function (identity, base64Payload) {
		var intensity = WL.decodeNexradIntensityBase64(base64Payload);
		var stats = WL.intensityStats(intensity);
		$scope.NexradPreview = {identity: identity, bounds: WL.parseNexradIdentity(identity), stats: stats};
		// Deferred to $timeout-free next digest via requestAnimationFrame so
		// the canvas element (ng-if'd on NexradPreview) exists in the DOM
		// first.
		if (typeof requestAnimationFrame === 'function') {
			requestAnimationFrame(function () {
				drawNexradCanvas('nexradPreviewCanvas', intensity);
			});
		}
	};

	function drawNexradCanvas(canvasId, intensity) {
		var canvas = document.getElementById(canvasId);
		if (!canvas || !intensity.length) {
			return;
		}
		var side = Math.ceil(Math.sqrt(intensity.length));
		canvas.width = side;
		canvas.height = side;
		var ctx = canvas.getContext('2d');
		if (!ctx) {
			return;
		}
		var img = ctx.createImageData(side, side);
		for (var i = 0; i < intensity.length; i++) {
			var level = Math.min(15, intensity[i]); // really only 4 bits - uatparse.NEXRADBlock's own doc comment
			var frac = level / 15;
			var idx = i * 4;
			img.data[idx] = Math.round(255 * frac);         // R: ramps up with intensity
			img.data[idx + 1] = Math.round(80 * (1 - frac)); // G: fades out
			img.data[idx + 2] = Math.round(255 * (1 - frac)); // B: fades from blue
			img.data[idx + 3] = level === 0 ? 0 : 255;        // fully transparent where no return at all
		}
		ctx.putImageData(img, 0, 0);
	}

	// --- cache status + inventory polling ---------------------------------------

	$scope.refreshCache = function () {
		$http.get(URL_FISBCACHE_STATUS_GET).
			then(function (response) {
				$scope.CacheStatus = response.data;
				$scope.CacheEnabled = !!response.data.enabled;
				$scope.CachePersistenceEnabled = !!response.data.persistenceEnabled;
			}, function (errorResponse) {
				// leave any previously-loaded status in place
			});
		$http.get(URL_FISBCACHE_INVENTORY_GET).
			then(function (response) {
				var fresh = WL.buildRows(response.data || []);
				$scope.Rows = WL.mergeRows(fresh, WL.rowsByIdentity($scope.Rows));
			}, function (errorResponse) {
				// leave any previously-loaded inventory in place - a
				// transient fetch error must never blank out already-known
				// cached weather (task section 13's "do not silently
				// remove useful cached information" requirement).
			});
	};

	var cacheRefreshInterval = $interval(function () {
		$scope.refreshCache();
	}, 3000); // matches web/plates/js/fisbcache.js's own polling cadence exactly

	// --- towers (matches web/plates/js/status.js's own polling exactly) --------

	function refreshTowers() {
		$http.get(URL_TOWERS_GET).
			then(function (response) {
				$scope.Towers = WL.countActiveTowers(response.data);
			}, function (errorResponse) {
				// nop - leave the last known count in place
			});
	}
	refreshTowers();
	var towersInterval = $interval(refreshTowers, 5000);

	// --- receiver-level status, via the existing /status websocket -------------
	// (1 Hz push - see main/managementinterface.go's handleStatusWS; reused
	// exactly as web/plates/js/status.js already does, no new polling loop)

	function connectStatusSocket() {
		if ($scope.statusSocket) {
			return;
		}
		var socket = new WebSocket(URL_STATUS_WS);
		$scope.statusSocket = socket;
		socket.onclose = function () {
			$scope.statusSocket = null;
			setTimeout(connectStatusSocket, 1000);
		};
		socket.onerror = function () {
			// onclose will fire next and handle reconnection
		};
		socket.onmessage = function (msg) {
			var status;
			try {
				status = JSON.parse(msg.data);
			} catch (e) {
				return;
			}
			var sum = WL.sumWeatherCounters(status);
			var now = Date.now();
			if (lastWeatherCounterSum === null || sum > lastWeatherCounterSum) {
				lastWeatherCounterAt = now;
			}
			lastWeatherCounterSum = sum;
			$scope.LastWeatherFrameAgeSeconds = lastWeatherCounterAt === null ? null : (now - lastWeatherCounterAt) / 1000;
			$scope.ReceiverFreshnessLabel = WL.receiverFreshnessLabel($scope.LastWeatherFrameAgeSeconds);
			$scope.ReceiverStatus = status; // raw /getStatus snapshot - only used for the NOTAM/SIGMET counter-only tab
			$scope.$apply();
		};
	}

	// --- live /weather text feed -------------------------------------------------

	function connectWeatherSocket() {
		if ($scope.weatherSocket) {
			return;
		}
		var socket = new WebSocket(URL_WEATHER_WS);
		$scope.weatherSocket = socket;
		$scope.WeatherConnectState = 'Disconnected';

		socket.onopen = function () {
			$scope.WeatherConnectState = 'Connected';
		};
		socket.onclose = function () {
			$scope.WeatherConnectState = 'Disconnected';
			$scope.weatherSocket = null;
			$scope.$apply();
			setTimeout(connectWeatherSocket, 1000);
		};
		socket.onerror = function () {
			$scope.WeatherConnectState = 'Problem';
			$scope.$apply();
		};
		socket.onmessage = function (msg) {
			var message;
			try {
				message = JSON.parse(msg.data);
			} catch (e) {
				return;
			}
			var now = Date.now();
			$scope.Rows = WL.upsertLiveRow($scope.Rows, message, now);
			var identity = message.Type + ' ' + message.Location;
			$scope.LiveTicker = WL.boundedUpsert($scope.LiveTicker,
				{identity: identity, category: WL.classifyReportType(message.Type), atMs: now},
				function (x) { return x.identity; }, MAX_LIVE_TICKER);
			$scope.$apply();
		};
	}

	$state.get('weather').onEnter = function () {
		connectWeatherSocket();
		connectStatusSocket();
	};

	$state.get('weather').onExit = function () {
		if ($scope.weatherSocket) {
			$scope.weatherSocket.close();
			$scope.weatherSocket = null;
		}
		if ($scope.statusSocket) {
			$scope.statusSocket.close();
			$scope.statusSocket = null;
		}
		$interval.cancel(cacheRefreshInterval);
		$interval.cancel(towersInterval);
	};

	// initial load
	$scope.refreshCache();
	connectWeatherSocket();
	connectStatusSocket();
}
