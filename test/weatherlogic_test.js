/*
weatherlogic_test.js: unit tests for web/plates/js/weatherlogic.js - the
Weather page's pure, dependency-free logic (categorization, freshness,
sorting/filtering, NEXRAD intensity decoding, bounded browser state).

No framework: uses Node's own built-in test runner and assert module
(both ship with Node itself - nothing to install, no config file). Run
with:

	node --test test/weatherlogic_test.js

or, on an older Node without --test's full feature set:

	node test/weatherlogic_test.js
*/
'use strict';

const assert = require('assert');
const path = require('path');
const WeatherLogic = require(path.join(__dirname, '..', 'web', 'plates', 'js', 'weatherlogic.js'));

// A tiny, dependency-free test harness (no framework) - falls back to
// this if the Node running the file is too old for node:test, and reads
// identically either way.
let test = null;
try {
	test = require('node:test').test;
} catch (e) {
	// fall through to the manual harness below
}

const results = {pass: 0, fail: 0};
function manualTest(name, fn) {
	try {
		fn();
		results.pass++;
		console.log('  ok - ' + name);
	} catch (err) {
		results.fail++;
		console.error('  NOT ok - ' + name);
		console.error('    ' + (err && err.message ? err.message : err));
	}
}
const t = test || manualTest;

// --- classifyReportType / splitIdentity -------------------------------------

t('classifyReportType: recognizes exactly the six cacheable text types', () => {
	assert.strictEqual(WeatherLogic.classifyReportType('METAR'), WeatherLogic.CATEGORY_METAR);
	assert.strictEqual(WeatherLogic.classifyReportType('SPECI'), WeatherLogic.CATEGORY_METAR);
	assert.strictEqual(WeatherLogic.classifyReportType('TAF'), WeatherLogic.CATEGORY_TAF);
	assert.strictEqual(WeatherLogic.classifyReportType('TAF.AMD'), WeatherLogic.CATEGORY_TAF);
	assert.strictEqual(WeatherLogic.classifyReportType('PIREP'), WeatherLogic.CATEGORY_PIREP);
	assert.strictEqual(WeatherLogic.classifyReportType('WINDS'), WeatherLogic.CATEGORY_WINDS);
});

t('classifyReportType: an unrecognized leading token is "other-text", never fabricated as NOTAM/SIGMET', () => {
	assert.strictEqual(WeatherLogic.classifyReportType('AIRMET'), WeatherLogic.CATEGORY_OTHER_TEXT);
	assert.strictEqual(WeatherLogic.classifyReportType('NOTAM'), WeatherLogic.CATEGORY_OTHER_TEXT);
	assert.strictEqual(WeatherLogic.classifyReportType(''), WeatherLogic.CATEGORY_OTHER_TEXT);
});

t('splitIdentity: splits "METAR KSEA" into type/location', () => {
	assert.deepStrictEqual(WeatherLogic.splitIdentity('METAR KSEA'), {type: 'METAR', location: 'KSEA'});
});

t('splitIdentity: a malformed identity degrades gracefully instead of throwing', () => {
	assert.deepStrictEqual(WeatherLogic.splitIdentity('NOSPACE'), {type: 'NOSPACE', location: ''});
	assert.deepStrictEqual(WeatherLogic.splitIdentity(''), {type: '', location: ''});
	assert.deepStrictEqual(WeatherLogic.splitIdentity(null), {type: '', location: ''});
});

// --- buildRows ---------------------------------------------------------------

t('buildRows: maps a realistic inventory item into a display row, content unknown', () => {
	const rows = WeatherLogic.buildRows([{
		productClass: 'text', identity: 'TAF KSEA', freshness: 'CACHED_FRESH',
		ageSeconds: 120.5, ageBasis: 'source', receivedAtUtc: '2026-09-27T23:20:00Z',
		sourceTimeUtc: '2026-09-27T23:20:00Z', sizeBytes: 126, sourceTrusted: true
	}]);
	assert.strictEqual(rows.length, 1);
	assert.strictEqual(rows[0].category, WeatherLogic.CATEGORY_TAF);
	assert.strictEqual(rows[0].location, 'KSEA');
	assert.strictEqual(rows[0].freshness, 'CACHED_FRESH');
	assert.strictEqual(rows[0].rawText, null);
	assert.strictEqual(rows[0].rawTextStatus, 'unavailable');
});

t('buildRows: a nexrad_tile item is categorized as NEXRAD with no location parsing attempted', () => {
	const rows = WeatherLogic.buildRows([{
		productClass: 'nexrad_tile',
		identity: 'radar=63;scale=0;lat=41.3333;lon=-95.2000;h=0.0667;w=0.8000',
		freshness: 'STALE', ageSeconds: 900, sizeBytes: 344
	}]);
	assert.strictEqual(rows[0].category, WeatherLogic.CATEGORY_NEXRAD);
});

t('buildRows: skips malformed items rather than guessing at them', () => {
	const rows = WeatherLogic.buildRows([{}, {identity: 'METAR KSEA'}, null, {productClass: 'text'}]);
	assert.strictEqual(rows.length, 0);
});

t('buildRows: an empty or missing inventory produces an empty array, never null/throw', () => {
	assert.deepStrictEqual(WeatherLogic.buildRows([]), []);
	assert.deepStrictEqual(WeatherLogic.buildRows(null), []);
	assert.deepStrictEqual(WeatherLogic.buildRows(undefined), []);
});

t('buildRows: an entry whose leading token is unrecognized is still cached, freshness UNSUPPORTED preserved as-is', () => {
	const rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'SOMETHINGNEW KSEA', freshness: 'UNSUPPORTED'}]);
	assert.strictEqual(rows[0].category, WeatherLogic.CATEGORY_OTHER_TEXT);
	assert.strictEqual(rows[0].freshness, 'UNSUPPORTED');
});

// --- merge / live text / fetched payload -------------------------------------

t('applyLiveText: sets rawText/rawTextStatus for the matching identity only, does not mutate the input array', () => {
	const rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'METAR KSEA', freshness: 'CACHED_FRESH'}]);
	const updated = WeatherLogic.applyLiveText(rows, 'METAR KSEA', 'METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000');
	assert.strictEqual(rows[0].rawText, null, 'original array must be untouched');
	assert.strictEqual(updated[0].rawText, 'METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000');
	assert.strictEqual(updated[0].rawTextStatus, 'live');
});

t('applyFetchedPayload: fills in unknown content but never overwrites a live observation with an older fetch', () => {
	let rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'METAR KSEA', freshness: 'CACHED_FRESH'}]);
	rows = WeatherLogic.applyLiveText(rows, 'METAR KSEA', 'LIVE TEXT');
	rows = WeatherLogic.applyFetchedPayload(rows, 'METAR KSEA', 'OLDER DISK TEXT');
	assert.strictEqual(rows[0].rawText, 'LIVE TEXT', 'a live observation must win over a disk readback');
	assert.strictEqual(rows[0].rawTextStatus, 'live');
});

t('applyFetchedPayload: fills content for a row that had none yet', () => {
	let rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'TAF KJAC', freshness: 'CACHED_AGING'}]);
	rows = WeatherLogic.applyFetchedPayload(rows, 'TAF KJAC', 'TAF KJAC 271720Z ...');
	assert.strictEqual(rows[0].rawText, 'TAF KJAC 271720Z ...');
	assert.strictEqual(rows[0].rawTextStatus, 'fetched');
});

t('markFetching / markFetchFailed: never override an already-known live or fetched row', () => {
	let rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'METAR KSEA', freshness: 'CACHED_FRESH'}]);
	rows = WeatherLogic.applyLiveText(rows, 'METAR KSEA', 'LIVE TEXT');
	rows = WeatherLogic.markFetching(rows, 'METAR KSEA');
	assert.strictEqual(rows[0].rawTextStatus, 'live');
	rows = WeatherLogic.markFetchFailed(rows, 'METAR KSEA');
	assert.strictEqual(rows[0].rawTextStatus, 'live');
});

t('markFetching / markFetchFailed: do apply to a row with no known content yet', () => {
	let rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'PIREP KABC', freshness: 'STALE'}]);
	rows = WeatherLogic.markFetching(rows, 'PIREP KABC');
	assert.strictEqual(rows[0].rawTextStatus, 'fetching');
	rows = WeatherLogic.markFetchFailed(rows, 'PIREP KABC');
	assert.strictEqual(rows[0].rawTextStatus, 'fetch-failed');
});

t('mergeRows: carries known rawText across a fresh inventory poll instead of discarding it', () => {
	let rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'METAR KSEA', freshness: 'CACHED_FRESH', ageSeconds: 30}]);
	rows = WeatherLogic.applyLiveText(rows, 'METAR KSEA', 'LIVE TEXT');
	const freshFromServer = WeatherLogic.buildRows([{productClass: 'text', identity: 'METAR KSEA', freshness: 'CACHED_FRESH', ageSeconds: 90}]);
	const merged = WeatherLogic.mergeRows(freshFromServer, WeatherLogic.rowsByIdentity(rows));
	assert.strictEqual(merged[0].rawText, 'LIVE TEXT');
	assert.strictEqual(merged[0].ageSeconds, 90, 'the fresh poll\'s own age must win, only content carries over');
});

// --- sorting / filtering ------------------------------------------------------

t('sortNewestFirst: orders by ageSeconds ascending, unknown age (null) sorts last', () => {
	const rows = [
		{identity: 'a', ageSeconds: 300},
		{identity: 'b', ageSeconds: 10},
		{identity: 'c', ageSeconds: null},
		{identity: 'd', ageSeconds: 60}
	];
	const sorted = WeatherLogic.sortNewestFirst(rows).map(r => r.identity);
	assert.deepStrictEqual(sorted, ['b', 'd', 'a', 'c']);
});

t('filterByStation: case-insensitive prefix match on location', () => {
	const rows = [{location: 'KSEA'}, {location: 'KPDX'}, {location: 'ksun'}];
	const filtered = WeatherLogic.filterByStation(rows, 'ks').map(r => r.location);
	assert.deepStrictEqual(filtered, ['KSEA', 'ksun']);
});

t('filterByStation: an empty query returns every row unchanged', () => {
	const rows = [{location: 'KSEA'}, {location: 'KPDX'}];
	assert.strictEqual(WeatherLogic.filterByStation(rows, '').length, 2);
	assert.strictEqual(WeatherLogic.filterByStation(rows, null).length, 2);
});

t('searchRawText: a row with unknown content never matches a non-empty query', () => {
	const rows = [{rawText: null}, {rawText: 'RWY 16 CLSD'}];
	const matched = WeatherLogic.searchRawText(rows, 'RWY');
	assert.strictEqual(matched.length, 1);
	assert.strictEqual(matched[0].rawText, 'RWY 16 CLSD');
});

t('rowsInCategory: partitions by category exactly', () => {
	const rows = [
		{category: WeatherLogic.CATEGORY_METAR}, {category: WeatherLogic.CATEGORY_TAF},
		{category: WeatherLogic.CATEGORY_METAR}
	];
	assert.strictEqual(WeatherLogic.rowsInCategory(rows, WeatherLogic.CATEGORY_METAR).length, 2);
	assert.strictEqual(WeatherLogic.rowsInCategory(rows, WeatherLogic.CATEGORY_TAF).length, 1);
	assert.strictEqual(WeatherLogic.rowsInCategory(rows, WeatherLogic.CATEGORY_PIREP).length, 0);
});

// --- age / freshness formatting ------------------------------------------------

t('formatAge: matches fisbcache.js\'s own three-band format exactly', () => {
	assert.strictEqual(WeatherLogic.formatAge(30), '30 s');
	assert.strictEqual(WeatherLogic.formatAge(90), '2 min');
	assert.strictEqual(WeatherLogic.formatAge(3 * 3600), '3.0 h');
	assert.strictEqual(WeatherLogic.formatAge(undefined), '?');
	assert.strictEqual(WeatherLogic.formatAge(NaN), '?');
});

t('receiverFreshnessLabel: no observation yet is NO WX FRAMES YET, not STALE', () => {
	assert.strictEqual(WeatherLogic.receiverFreshnessLabel(null), 'NO WX FRAMES YET');
	assert.strictEqual(WeatherLogic.receiverFreshnessLabel(undefined), 'NO WX FRAMES YET');
});

t('receiverFreshnessLabel: matches the e-paper dashboard\'s exact 5 min / 15 min thresholds', () => {
	assert.strictEqual(WeatherLogic.receiverFreshnessLabel(0), 'WX RX RECENT');
	assert.strictEqual(WeatherLogic.receiverFreshnessLabel(5 * 60), 'WX RX RECENT');
	assert.strictEqual(WeatherLogic.receiverFreshnessLabel(5 * 60 + 1), 'WX RX AGING');
	assert.strictEqual(WeatherLogic.receiverFreshnessLabel(15 * 60), 'WX RX AGING');
	assert.strictEqual(WeatherLogic.receiverFreshnessLabel(15 * 60 + 1), 'WX RX STALE');
});

t('sumWeatherCounters: sums exactly the five fields epaper ProductTotals.weather() sums, excludes NOTAM/OTHER', () => {
	const status = {
		UAT_METAR_total: 10, UAT_TAF_total: 21, UAT_NEXRAD_total: 218,
		UAT_SIGMET_total: 0, UAT_PIREP_total: 1,
		UAT_NOTAM_total: 14, UAT_OTHER_total: 101 // must be excluded
	};
	assert.strictEqual(WeatherLogic.sumWeatherCounters(status), 10 + 21 + 218 + 0 + 1);
});

t('sumWeatherCounters: a missing/null status sums to zero, never throws', () => {
	assert.strictEqual(WeatherLogic.sumWeatherCounters(null), 0);
	assert.strictEqual(WeatherLogic.sumWeatherCounters({}), 0);
});

t('countActiveTowers: matches status.js\'s own Messages_last_minute > 0 rule exactly', () => {
	const towers = {
		'(47.8,-116.9)': {Messages_last_minute: 11},
		'(47.9,-117.0)': {Messages_last_minute: 0}
	};
	assert.strictEqual(WeatherLogic.countActiveTowers(towers), 1);
	assert.strictEqual(WeatherLogic.countActiveTowers({}), 0);
	assert.strictEqual(WeatherLogic.countActiveTowers(null), 0);
});

// --- NEXRAD intensity decoding -------------------------------------------------

t('decodeNexradIntensityBase64: round-trips a big-endian uint16 array through base64 exactly', () => {
	const intensity = [0, 3, 7, 15, 2, 0, 9];
	const buf = Buffer.alloc(intensity.length * 2);
	intensity.forEach((v, i) => buf.writeUInt16BE(v, i * 2));
	const decoded = WeatherLogic.decodeNexradIntensityBase64(buf.toString('base64'));
	assert.deepStrictEqual(decoded, intensity);
});

t('intensityStats: reports count/max/nonZero correctly, including the all-zero case', () => {
	assert.deepStrictEqual(WeatherLogic.intensityStats([0, 3, 7, 15, 2, 0]), {count: 6, max: 15, nonZero: 4});
	assert.deepStrictEqual(WeatherLogic.intensityStats([0, 0, 0]), {count: 3, max: 0, nonZero: 0});
	assert.deepStrictEqual(WeatherLogic.intensityStats([]), {count: 0, max: 0, nonZero: 0});
});

t('parseNexradIdentity: reverses fisbcache.NexradTileIdentity\'s exact format', () => {
	const parsed = WeatherLogic.parseNexradIdentity('radar=63;scale=0;lat=41.3333;lon=-95.2000;h=0.0667;w=0.8000');
	assert.deepStrictEqual(parsed, {radar: '63', scale: '0', lat: '41.3333', lon: '-95.2000', h: '0.0667', w: '0.8000'});
});

t('parseNexradIdentity: an empty/missing identity yields an empty object, never throws', () => {
	assert.deepStrictEqual(WeatherLogic.parseNexradIdentity(''), {});
	assert.deepStrictEqual(WeatherLogic.parseNexradIdentity(null), {});
});

// --- live /weather messages ------------------------------------------------------

t('parseShortDatetimeUTC: parses a DDHHMM token against the current UTC calendar day', () => {
	const now = Date.UTC(2026, 8, 27, 23, 30, 0); // 2026-09-27T23:30:00Z
	const d = WeatherLogic.parseShortDatetimeUTC('271853', now);
	assert.strictEqual(d.getUTCDate(), 27);
	assert.strictEqual(d.getUTCHours(), 18);
	assert.strictEqual(d.getUTCMinutes(), 53);
});

t('parseShortDatetimeUTC: a TAF-style DDHH token (no minutes) defaults minutes to 0', () => {
	const now = Date.UTC(2026, 8, 27, 23, 30, 0);
	const d = WeatherLogic.parseShortDatetimeUTC('2718', now);
	assert.strictEqual(d.getUTCMinutes(), 0);
});

t('parseShortDatetimeUTC: a too-short token returns null rather than a garbage date', () => {
	assert.strictEqual(WeatherLogic.parseShortDatetimeUTC('12', Date.now()), null);
	assert.strictEqual(WeatherLogic.parseShortDatetimeUTC('', Date.now()), null);
});

t('upsertLiveRow: updates an existing cache-backed row\'s content without discarding its cache-derived freshness state entirely', () => {
	const now = Date.UTC(2026, 8, 27, 23, 30, 0);
	let rows = WeatherLogic.buildRows([{productClass: 'text', identity: 'METAR KSEA', freshness: 'CACHED_AGING', ageSeconds: 400}]);
	rows = WeatherLogic.upsertLiveRow(rows, {Type: 'METAR', Location: 'KSEA', Time: '271830', Data: 'METAR KSEA 271830Z AUTO 00000KT 10SM CLR 15/10 A3000'}, now);
	assert.strictEqual(rows.length, 1, 'must update the existing row, not duplicate it');
	assert.strictEqual(rows[0].rawText, 'METAR KSEA 271830Z AUTO 00000KT 10SM CLR 15/10 A3000');
	assert.strictEqual(rows[0].rawTextStatus, 'live');
});

t('upsertLiveRow: synthesizes a new LIVE row when the cache has no backing entry (e.g. cache disabled)', () => {
	const now = Date.UTC(2026, 8, 27, 23, 30, 0);
	let rows = WeatherLogic.buildRows([]); // empty inventory - cache disabled or not yet polled
	rows = WeatherLogic.upsertLiveRow(rows, {Type: 'TAF', Location: 'KJAC', Time: '271720', Data: 'TAF KJAC 271720Z ...'}, now);
	assert.strictEqual(rows.length, 1);
	assert.strictEqual(rows[0].identity, 'TAF KJAC');
	assert.strictEqual(rows[0].category, WeatherLogic.CATEGORY_TAF);
	assert.strictEqual(rows[0].freshness, 'LIVE');
	assert.strictEqual(rows[0].rawTextStatus, 'live');
});

t('upsertLiveRow: a message missing Type/Location is ignored rather than corrupting the row list', () => {
	const rows = WeatherLogic.buildRows([]);
	assert.deepStrictEqual(WeatherLogic.upsertLiveRow(rows, {}, Date.now()), rows);
	assert.deepStrictEqual(WeatherLogic.upsertLiveRow(rows, null, Date.now()), rows);
});

// --- bounded browser state ------------------------------------------------------

t('boundedUpsert: caps length and de-duplicates by key, newest first', () => {
	let list = [];
	list = WeatherLogic.boundedUpsert(list, {id: 1}, x => x.id, 3);
	list = WeatherLogic.boundedUpsert(list, {id: 2}, x => x.id, 3);
	list = WeatherLogic.boundedUpsert(list, {id: 3}, x => x.id, 3);
	list = WeatherLogic.boundedUpsert(list, {id: 4}, x => x.id, 3);
	assert.deepStrictEqual(list.map(x => x.id), [4, 3, 2]);
	assert.strictEqual(list.length, 3, 'must never exceed maxLen regardless of how many items are pushed');
});

t('boundedUpsert: re-upserting an existing key moves it to the front without growing the list', () => {
	let list = [{id: 1, v: 'a'}, {id: 2, v: 'b'}];
	list = WeatherLogic.boundedUpsert(list, {id: 1, v: 'updated'}, x => x.id, 5);
	assert.deepStrictEqual(list, [{id: 1, v: 'updated'}, {id: 2, v: 'b'}]);
});

if (!test) {
	console.log('\n' + results.pass + ' passed, ' + results.fail + ' failed');
	process.exit(results.fail > 0 ? 1 : 0);
}
