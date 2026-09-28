/*
weatherlogic.js: pure, dependency-free logic for the Weather page
(weather.js/weather.html). No AngularJS, no DOM, no network access -
every function here takes plain data in and returns plain data out, so
this file can be unit-tested directly with a plain `node` invocation
(see test/weatherlogic_test.js) without adding any JS test framework to
the project.

weather.js (the AngularJS controller) is the only caller of this file in
production; it owns all HTTP/WebSocket/DOM/interval concerns and passes
this module's functions whatever plain data those concerns produce.
*/
(function (root, factory) {
	if (typeof module !== 'undefined' && module.exports) {
		module.exports = factory();
	} else {
		root.WeatherLogic = factory();
	}
}(typeof self !== 'undefined' ? self : this, function () {
	'use strict';

	// --- product categorization --------------------------------------------
	//
	// This is the SAME six leading tokens fisbcache/policy.go's textPolicies
	// recognizes (METAR, SPECI, TAF, TAF.AMD, WINDS, PIREP) - the only text
	// report types this codebase actually decodes/caches (see
	// docs/fisb-weather-cache.md and main/fisbcachecapture.go's
	// fisbParseTextReportHeader). A leading token outside this list is still
	// a real, receivable text report (fisbcache stores it as ClassText with
	// freshness "UNSUPPORTED"), never a NOTAM/SIGMET/AIRMET/lightning/
	// turbulence/icing decode - none of those are decoded anywhere in this
	// codebase (see the "not supported" section of the doc). This module
	// must never claim otherwise.
	var CATEGORY_METAR = 'metar';
	var CATEGORY_TAF = 'taf';
	var CATEGORY_PIREP = 'pirep';
	var CATEGORY_WINDS = 'winds';
	var CATEGORY_NEXRAD = 'nexrad';
	var CATEGORY_OTHER_TEXT = 'other-text';

	// classifyReportType maps a text report's own leading token (the
	// "Type" field of a /weather WeatherMessage, or the first word of an
	// inventory item's Identity) to a display category.
	function classifyReportType(type) {
		switch (type) {
			case 'METAR':
			case 'SPECI':
				return CATEGORY_METAR;
			case 'TAF':
			case 'TAF.AMD':
				return CATEGORY_TAF;
			case 'PIREP':
				return CATEGORY_PIREP;
			case 'WINDS':
				return CATEGORY_WINDS;
			default:
				return CATEGORY_OTHER_TEXT;
		}
	}

	// splitIdentity splits a fisbcache text-class Identity ("METAR KSEA")
	// into {type, location} - the exact inverse of fisbcache.TextKey. A
	// malformed identity (no space) degrades to {type: identity, location:
	// ''} rather than throwing, since this only ever describes already-
	// validated server data, but a defensive caller should never crash on
	// something it merely displays.
	function splitIdentity(identity) {
		if (!identity) {
			return {type: '', location: ''};
		}
		var i = identity.indexOf(' ');
		if (i < 0) {
			return {type: identity, location: ''};
		}
		return {type: identity.substring(0, i), location: identity.substring(i + 1)};
	}

	// --- inventory rows ------------------------------------------------------
	//
	// A "row" is this page's own normalized view of one cache entry,
	// independent of whether its raw content is known yet. Building rows
	// never fabricates content: rawText/rawTextStatus start out
	// 'unavailable' and are only ever set by applyLiveText/
	// applyFetchedPayload, both driven by a real server response.

	// buildRows converts /getFISBCacheInventory's own items (productClass,
	// identity, freshness, ageSeconds, ageBasis, receivedAtUtc,
	// sourceTimeUtc, sizeBytes, sourceTrusted) into display rows. Unknown
	// or malformed items are skipped, never guessed at.
	function buildRows(inventoryItems) {
		var rows = [];
		if (!inventoryItems) {
			return rows;
		}
		for (var i = 0; i < inventoryItems.length; i++) {
			var item = inventoryItems[i];
			if (!item || !item.identity || !item.productClass) {
				continue;
			}
			var category;
			var location = '';
			if (item.productClass === 'nexrad_tile') {
				category = CATEGORY_NEXRAD;
			} else {
				var parts = splitIdentity(item.identity);
				category = classifyReportType(parts.type);
				location = parts.location;
			}
			rows.push({
				productClass: item.productClass,
				identity: item.identity,
				category: category,
				location: location,
				freshness: item.freshness || 'UNSUPPORTED',
				ageSeconds: (typeof item.ageSeconds === 'number') ? item.ageSeconds : null,
				ageBasis: item.ageBasis || null,
				receivedAtUtc: item.receivedAtUtc || null,
				sourceTimeUtc: item.sourceTimeUtc || null,
				sizeBytes: (typeof item.sizeBytes === 'number') ? item.sizeBytes : null,
				sourceTrusted: !!item.sourceTrusted,
				rawText: null,
				rawTextStatus: 'unavailable' // 'unavailable' | 'live' | 'fetched' | 'fetching' | 'fetch-failed'
			});
		}
		return rows;
	}

	// mergeRows combines a freshly-built row array with whatever
	// rawText/rawTextStatus this browser session already knows for the
	// same identity (from a live /weather message or a prior
	// /getFISBCachePayload fetch) - so a page refresh of the inventory
	// alone never discards content this session already has. Returns a
	// NEW array; never mutates either input.
	function mergeRows(freshRows, priorRowsByIdentity) {
		if (!priorRowsByIdentity) {
			return freshRows;
		}
		return freshRows.map(function (row) {
			var prior = priorRowsByIdentity[row.identity];
			if (prior && prior.rawText != null) {
				var merged = shallowCopy(row);
				merged.rawText = prior.rawText;
				merged.rawTextStatus = prior.rawTextStatus;
				return merged;
			}
			return row;
		});
	}

	function rowsByIdentity(rows) {
		var out = {};
		for (var i = 0; i < rows.length; i++) {
			out[rows[i].identity] = rows[i];
		}
		return out;
	}

	// applyLiveText marks the row for `identity` (if present) with text
	// observed live, over /weather, during this browser session - the
	// strongest, freshest source, since it is exactly what was just
	// received, not a disk readback. Returns a NEW array.
	function applyLiveText(rows, identity, text) {
		return rows.map(function (row) {
			if (row.identity !== identity) {
				return row;
			}
			var merged = shallowCopy(row);
			merged.rawText = text;
			merged.rawTextStatus = 'live';
			return merged;
		});
	}

	// applyFetchedPayload marks the row for `identity` with content
	// retrieved on demand from /getFISBCachePayload (a persisted-file
	// readback - see main/fisbcachepayload.go). Never overwrites a 'live'
	// row's text with older fetched content - live observation during
	// this session is always at least as good as a disk readback.
	function applyFetchedPayload(rows, identity, text) {
		return rows.map(function (row) {
			if (row.identity !== identity || row.rawTextStatus === 'live') {
				return row;
			}
			var merged = shallowCopy(row);
			merged.rawText = text;
			merged.rawTextStatus = 'fetched';
			return merged;
		});
	}

	// markFetchFailed/markFetching let the UI show an honest, transient
	// "checking.../not available" state around an on-demand fetch,
	// without ever inventing content.
	function markFetching(rows, identity) {
		return rows.map(function (row) {
			if (row.identity !== identity || row.rawTextStatus === 'live' || row.rawTextStatus === 'fetched') {
				return row;
			}
			var merged = shallowCopy(row);
			merged.rawTextStatus = 'fetching';
			return merged;
		});
	}

	function markFetchFailed(rows, identity) {
		return rows.map(function (row) {
			if (row.identity !== identity || row.rawTextStatus === 'live' || row.rawTextStatus === 'fetched') {
				return row;
			}
			var merged = shallowCopy(row);
			merged.rawTextStatus = 'fetch-failed';
			return merged;
		});
	}

	function shallowCopy(obj) {
		var out = {};
		for (var k in obj) {
			if (Object.prototype.hasOwnProperty.call(obj, k)) {
				out[k] = obj[k];
			}
		}
		return out;
	}

	// --- sorting / filtering --------------------------------------------------

	function sortNewestFirst(rows) {
		return rows.slice().sort(function (a, b) {
			var ageA = (a.ageSeconds == null) ? Infinity : a.ageSeconds;
			var ageB = (b.ageSeconds == null) ? Infinity : b.ageSeconds;
			return ageA - ageB;
		});
	}

	function sortByStation(rows) {
		return rows.slice().sort(function (a, b) {
			return (a.location || '').localeCompare(b.location || '');
		});
	}

	// filterByStation keeps rows whose location starts with `query`
	// (case-insensitive) - a plain prefix match, deliberately not a full
	// text search, since a station identifier is what a pilot actually
	// types (e.g. "KSEA").
	function filterByStation(rows, query) {
		if (!query) {
			return rows;
		}
		var q = query.toUpperCase();
		return rows.filter(function (row) {
			return (row.location || '').toUpperCase().indexOf(q) === 0;
		});
	}

	// searchRawText keeps rows whose known raw text contains `query`
	// (case-insensitive substring). A row with no known raw text yet
	// never matches a non-empty query - it is never treated as a hit just
	// because its content is unknown.
	function searchRawText(rows, query) {
		if (!query) {
			return rows;
		}
		var q = query.toUpperCase();
		return rows.filter(function (row) {
			return !!(row.rawText && row.rawText.toUpperCase().indexOf(q) >= 0);
		});
	}

	function rowsInCategory(rows, category) {
		return rows.filter(function (row) {
			return row.category === category;
		});
	}

	// --- age / freshness formatting -------------------------------------------

	// formatAge mirrors fisbcache.js's own $scope.fmtAge exactly (same
	// three bands: seconds/minutes/hours) - the Weather Cache diagnostics
	// page and this Weather page must never show two different age
	// formats for the same underlying ageSeconds value.
	function formatAge(seconds) {
		if (seconds === undefined || seconds === null || isNaN(seconds)) {
			return '?';
		}
		if (seconds < 60) {
			return Math.round(seconds) + ' s';
		}
		if (seconds < 7200) {
			return Math.round(seconds / 60) + ' min';
		}
		return (seconds / 3600).toFixed(1) + ' h';
	}

	// DEFAULT_RECEIVER_FRESHNESS_THRESHOLDS mirrors, in seconds, exactly
	// epaper/dashboard_derive.go's DefaultThresholds().WXCurrentWithin (5
	// min) and .WXAgingWithin (15 min) - the two numbers behind the
	// e-paper dashboard's "WX RX RECENT"/"WX RX AGING"/"WX RX STALE"
	// labels. They cannot be called directly from a browser (that logic
	// lives in an unexported Go method), so this page reuses the exact
	// same, already-documented threshold values and label text instead of
	// inventing a second, different freshness vocabulary - see
	// docs/fisb-weather-cache.md and docs/epaper-operating-dashboard.md's
	// own RECENT/AGING/STALE table for the shared source of truth.
	var DEFAULT_RECEIVER_FRESHNESS_THRESHOLDS = {
		recentSeconds: 5 * 60,
		agingSeconds: 15 * 60
	};

	// receiverFreshnessLabel classifies RECEIVER-LEVEL freshness (time
	// since the newest weather-product FRAME was decoded, regardless of
	// any individual product's own age) - deliberately the same concept,
	// computed from the same counters, as epaper/dashboard_derive.go's
	// fisbTile. secondsSinceLastFrame is null/undefined when no
	// weather-product frame has ever been observed this boot.
	function receiverFreshnessLabel(secondsSinceLastFrame, thresholds) {
		var t = thresholds || DEFAULT_RECEIVER_FRESHNESS_THRESHOLDS;
		if (secondsSinceLastFrame === null || secondsSinceLastFrame === undefined) {
			return 'NO WX FRAMES YET';
		}
		if (secondsSinceLastFrame <= t.recentSeconds) {
			return 'WX RX RECENT';
		}
		if (secondsSinceLastFrame <= t.agingSeconds) {
			return 'WX RX AGING';
		}
		return 'WX RX STALE';
	}

	// --- receiver-level counters (from /getStatus, polled) ---------------------

	// WEATHER_COUNTER_FIELDS mirrors epaper's ProductTotals.weather()
	// exactly (main package globalStatus fields UAT_METAR_total,
	// UAT_TAF_total, UAT_NEXRAD_total, UAT_SIGMET_total, UAT_PIREP_total -
	// NOTAM and OTHER are deliberately excluded from this particular sum,
	// matching epaper/dashboard_derive.go:98-100 exactly).
	var WEATHER_COUNTER_FIELDS = [
		'UAT_METAR_total', 'UAT_TAF_total', 'UAT_NEXRAD_total',
		'UAT_SIGMET_total', 'UAT_PIREP_total'
	];

	function sumWeatherCounters(status) {
		if (!status) {
			return 0;
		}
		var sum = 0;
		for (var i = 0; i < WEATHER_COUNTER_FIELDS.length; i++) {
			var v = status[WEATHER_COUNTER_FIELDS[i]];
			sum += (typeof v === 'number') ? v : 0;
		}
		return sum;
	}

	// countActiveTowers mirrors web/plates/js/status.js's own getTowers()
	// exactly: an entry in /getTowers counts as active when its
	// Messages_last_minute is greater than zero.
	function countActiveTowers(towers) {
		var count = 0;
		if (!towers) {
			return count;
		}
		for (var key in towers) {
			if (Object.prototype.hasOwnProperty.call(towers, key) && towers[key] && towers[key].Messages_last_minute > 0) {
				count++;
			}
		}
		return count;
	}

	// --- NEXRAD intensity decoding ---------------------------------------------

	// decodeNexradIntensityBase64 reverses fisbEncodeNexradPayload
	// (main/fisbcachecapture.go): base64 text -> big-endian uint16 array.
	// Works in both a browser (atob) and Node (Buffer) - only the base64
	// decode primitive differs; the byte-pair reassembly is identical.
	function decodeNexradIntensityBase64(b64) {
		var bytes;
		if (typeof atob === 'function') {
			var bin = atob(b64);
			bytes = new Uint8Array(bin.length);
			for (var i = 0; i < bin.length; i++) {
				bytes[i] = bin.charCodeAt(i);
			}
		} else {
			bytes = Uint8Array.from(Buffer.from(b64, 'base64'));
		}
		var out = new Array(Math.floor(bytes.length / 2));
		for (var j = 0; j < out.length; j++) {
			out[j] = (bytes[j * 2] << 8) | bytes[j * 2 + 1];
		}
		return out;
	}

	// intensityStats summarizes a decoded intensity array for a metadata-
	// only view (task section 10's minimum requirement) even when no
	// canvas rendering is attempted - each value is really only 4 bits
	// (0-15) per uatparse.NEXRADBlock's own doc comment.
	function intensityStats(values) {
		var max = 0;
		var nonZero = 0;
		for (var i = 0; i < values.length; i++) {
			if (values[i] > max) {
				max = values[i];
			}
			if (values[i] > 0) {
				nonZero++;
			}
		}
		return {count: values.length, max: max, nonZero: nonZero};
	}

	// parseNexradIdentity reverses fisbcache.NexradTileIdentity's fixed
	// "radar=%d;scale=%d;lat=%.4f;lon=%.4f;h=%.4f;w=%.4f" format - no
	// escaping needed, matching that function's own doc comment, since a
	// value in this format never contains ';' or '='.
	function parseNexradIdentity(identity) {
		var out = {};
		if (!identity) {
			return out;
		}
		var parts = identity.split(';');
		for (var i = 0; i < parts.length; i++) {
			var kv = parts[i].split('=');
			if (kv.length === 2) {
				out[kv[0]] = kv[1];
			}
		}
		return out;
	}

	// --- live /weather messages (no cache backing required) --------------------
	//
	// The /weather websocket (main/gen_gdl90.go's WeatherMessage) works
	// whether or not the FIS-B rolling cache is enabled at all - it is
	// this codebase's original live text-report feed. This page must
	// therefore be able to show a freshly-arrived report even with the
	// cache disabled or an inventory row that doesn't exist yet, not only
	// update an existing cache-backed row.

	// parseShortDatetimeUTC mirrors weather.js's PREVIOUS implementation
	// (parseShortDatetime) exactly: a FIS-B short date-time token is
	// "DDHHMM" (day/hour/minute) or, for some TAF forms, "DDHH" (no
	// minutes) - there is no year or month, so this always reconstructs
	// against the UTC calendar day/hour/minute of `nowMs`, exactly as the
	// previous page already did (deliberately NOT LocaltimeReceived,
	// which is stratuxClock time - monotonic-only, not wall-clock - see
	// docs/fisb-weather-cache.md's time-model section). Returns null for
	// a token too short to be one of these forms, rather than guessing.
	function parseShortDatetimeUTC(token, nowMs) {
		var s = String(token || '');
		if (s.length < 4) {
			return null;
		}
		var d = new Date(nowMs);
		d.setUTCDate(parseInt(s.substring(0, 2), 10));
		d.setUTCHours(parseInt(s.substring(2, 4), 10));
		d.setUTCMinutes(s.length > 4 ? parseInt(s.substring(4, 6), 10) : 0);
		d.setUTCSeconds(0);
		d.setUTCMilliseconds(0);
		return d;
	}

	// upsertLiveRow folds one live WeatherMessage ({Type, Location, Time,
	// Data}) into `rows`: updates the matching row's content if the
	// identity already exists (from a cache-backed poll), or synthesizes
	// a new row (freshness "LIVE", matching fisbcache.js's own
	// freshnessClass "LIVE" case) when it does not - the case where the
	// rolling cache is disabled, or simply hasn't polled this identity
	// yet. Returns a NEW array; never mutates its input.
	function upsertLiveRow(rows, message, nowMs) {
		if (!message || !message.Type || !message.Location) {
			return rows;
		}
		var identity = message.Type + ' ' + message.Location;
		var when = parseShortDatetimeUTC(message.Time, nowMs);
		var ageSeconds = when ? Math.max(0, (nowMs - when.getTime()) / 1000) : null;

		var found = false;
		var out = rows.map(function (row) {
			if (row.identity !== identity) {
				return row;
			}
			found = true;
			var merged = shallowCopy(row);
			merged.rawText = message.Data || '';
			merged.rawTextStatus = 'live';
			if (ageSeconds !== null) {
				merged.ageSeconds = ageSeconds;
				merged.ageBasis = 'source';
			}
			return merged;
		});
		if (!found) {
			out = out.concat([{
				productClass: 'text',
				identity: identity,
				category: classifyReportType(message.Type),
				location: message.Location,
				freshness: 'LIVE',
				ageSeconds: ageSeconds,
				ageBasis: 'source',
				receivedAtUtc: null,
				sourceTimeUtc: null,
				sizeBytes: (message.Data || '').length,
				sourceTrusted: false,
				rawText: message.Data || '',
				rawTextStatus: 'live'
			}]);
		}
		return out;
	}

	// --- bounded, deduplicated lists (browser memory bound) --------------------

	// boundedUpsert inserts/updates `item` (matched by keyFn) at the front
	// of `list`, capped at maxLen - the general mechanism this page uses
	// anywhere it keeps a rolling, in-memory list fed by a live socket, so
	// the browser's own state can never grow without bound regardless of
	// how long the page stays open (task section 15/16's requirement).
	// Returns a NEW array.
	function boundedUpsert(list, item, keyFn, maxLen) {
		var key = keyFn(item);
		var out = [item];
		for (var i = 0; i < list.length; i++) {
			if (keyFn(list[i]) !== key) {
				out.push(list[i]);
			}
		}
		if (out.length > maxLen) {
			out = out.slice(0, maxLen);
		}
		return out;
	}

	return {
		CATEGORY_METAR: CATEGORY_METAR,
		CATEGORY_TAF: CATEGORY_TAF,
		CATEGORY_PIREP: CATEGORY_PIREP,
		CATEGORY_WINDS: CATEGORY_WINDS,
		CATEGORY_NEXRAD: CATEGORY_NEXRAD,
		CATEGORY_OTHER_TEXT: CATEGORY_OTHER_TEXT,
		DEFAULT_RECEIVER_FRESHNESS_THRESHOLDS: DEFAULT_RECEIVER_FRESHNESS_THRESHOLDS,

		classifyReportType: classifyReportType,
		splitIdentity: splitIdentity,
		parseShortDatetimeUTC: parseShortDatetimeUTC,
		upsertLiveRow: upsertLiveRow,
		buildRows: buildRows,
		mergeRows: mergeRows,
		rowsByIdentity: rowsByIdentity,
		applyLiveText: applyLiveText,
		applyFetchedPayload: applyFetchedPayload,
		markFetching: markFetching,
		markFetchFailed: markFetchFailed,
		sortNewestFirst: sortNewestFirst,
		sortByStation: sortByStation,
		filterByStation: filterByStation,
		searchRawText: searchRawText,
		rowsInCategory: rowsInCategory,
		formatAge: formatAge,
		receiverFreshnessLabel: receiverFreshnessLabel,
		sumWeatherCounters: sumWeatherCounters,
		countActiveTowers: countActiveTowers,
		decodeNexradIntensityBase64: decodeNexradIntensityBase64,
		intensityStats: intensityStats,
		parseNexradIdentity: parseNexradIdentity,
		boundedUpsert: boundedUpsert
	};
}));
