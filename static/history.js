// Device history charts from /api/metrics/history: CPU, temperature, disk use
// and sensor gauges (voltage, current), each shown only when the device has it.
// Plain SVG, no chart library. Names from SNMP (mount paths, sensors) are set
// with textContent, never innerHTML.
(function () {
    const base = window.location.pathname.includes('/snmp') ? '/snmp' : '';
    const SVG_NS = 'http://www.w3.org/2000/svg';
    const MAX_SERIES = 6; // colour slots --s1..--s6, assigned in order
    const HEIGHT = 220;
    const MARGIN = { top: 10, bottom: 26, left: 40 };
    // Mounts that aren't real disks
    const VIRTUAL_MOUNT = /^\/(dev|run|sys|proc|var\/run)(\/|$)|^\/tmp$/;

    const dialog = document.getElementById('historyDialog');
    const titleEl = document.getElementById('historyTitle');
    const subEl = document.getElementById('historySub');
    const rangeGroup = document.getElementById('historyRange');
    const tempUnitGroup = document.getElementById('tempUnit');
    const statusEl = document.getElementById('historyStatus');
    const grid = document.getElementById('historyCharts');
    const liveEl = document.getElementById('historyLive');
    const WEAK_SNR_DB = 20; // below this a Wi-Fi client's link is marginal
    const LOW_SUPPLY_PCT = 15; // about where printers start warning
    // How each sensor unit is charted, in display order; any other unit is
    // charted under its own name. unit is the suffix on values (default " <key>"),
    // signed lets the axis go below zero, and rank picks which series get the
    // colour slots when there are more than MAX_SERIES (the rest go in the table).
    const SENSOR_CHARTS = {
        toner: { title: 'Toner', sub: '% remaining per cartridge', unit: '%', decimals: 0, yFixed: [0, 100] },
        life: { title: 'Parts life', sub: '% life remaining per part', unit: '%', decimals: 1, yFixed: [0, 100], rank: 'low' },
        pages: { title: 'Pages printed', sub: 'Lifetime page count', unit: ' pages', decimals: 0 },
        clients: { title: 'Clients per radio', sub: 'Connected Wi-Fi clients', unit: ' clients', decimals: 0 },
        ssid_clients: { title: 'Clients per SSID', sub: 'Connected Wi-Fi clients', unit: ' clients', decimals: 0 },
        client_mbps: { title: 'Client usage', sub: 'Mbps per client, both directions', unit: ' Mbps', rank: 'high' },
        snr: { title: 'Client signal', sub: 'Signal-to-noise per client, dB (higher is better)', unit: ' dB', decimals: 0, rank: 'low' },
        airtime: { title: 'Airtime', sub: '% of airtime in use per radio', unit: '%', decimals: 0 },
        dBm: { title: 'Noise floor', sub: 'dBm per radio (lower is better)', decimals: 0, signed: true },
        mem_free: { title: 'Memory free', sub: '% of memory free', unit: '%', decimals: 0 },
        V: { title: 'Voltage', sub: 'Volts per input' },
        A: { title: 'Port current', sub: 'Amps drawn per power port' },
        W: { title: 'Power', sub: 'Watts per sensor' },
        RPM: { title: 'Fan speed', sub: 'RPM per fan', decimals: 0 },
    };
    // Interface traffic arrives as sensors "<interface> in" / "<interface> out"
    const TRAFFIC_UNIT = 'Mbps';

    let currentIP = '';
    let currentRange = load('snmpHistoryRange', '24h');
    let tempUnit = load('snmpTempUnit', 'F') === 'C' ? 'C' : 'F'; // temperatures are stored in °C
    let lastData = null;
    let refreshTimer = null;

    function load(key, fallback) {
        try { return localStorage.getItem(key) || fallback; } catch (e) { return fallback; }
    }
    function save(key, value) {
        try { localStorage.setItem(key, value); } catch (e) { /* private mode */ }
    }

    function el(tag, cls, text) {
        const e = document.createElement(tag);
        if (cls) e.className = cls;
        if (text !== undefined) e.textContent = text;
        return e;
    }
    // stroke and fill go through style because presentation attributes can't use var()
    function svgEl(tag, attrs) {
        const e = document.createElementNS(SVG_NS, tag);
        for (const k in attrs) {
            if (k === 'stroke' || k === 'fill') e.style[k] = attrs[k];
            else e.setAttribute(k, attrs[k]);
        }
        return e;
    }

    // ---- popup -------------------------------------------------------------

    // Called by app.js when a device in the Network Devices list is clicked
    window.openDeviceHistory = function (ip, name) {
        if (ip !== currentIP) {
            currentIP = ip;
            lastData = null;
            grid.replaceChildren();
            clearLive();
        }
        titleEl.textContent = name || ip;
        subEl.textContent = name ? ip : '';
        statusEl.textContent = 'Loading…';
        if (!dialog.open) dialog.showModal();
        fetchHistory();
    };

    // ---- data --------------------------------------------------------------

    function fetchHistory() {
        clearTimeout(refreshTimer);
        if (!currentIP || !dialog.open) return;
        // Keep the previous charts visible, dimmed, while new data loads
        grid.classList.add('refreshing');
        const ip = currentIP, range = currentRange;
        fetchLive(ip);
        fetch(`${base}/api/metrics/history?ip=${encodeURIComponent(ip)}&range=${range}`)
            .then(res => res.json().then(body => ({ ok: res.ok, body })))
            .then(result => {
                if (ip !== currentIP || range !== currentRange) return;
                if (!result.ok) {
                    statusEl.textContent = result.body.error || 'Could not load history';
                    lastData = null;
                    renderAll();
                    return;
                }
                lastData = result.body;
                statusEl.textContent = lastData.resolution === 'hourly'
                    ? 'Hourly averages' : '1-minute samples';
                renderAll();
            })
            .catch(err => {
                statusEl.textContent = 'Could not load history';
                console.error('History fetch failed', err);
            })
            .finally(() => {
                if (ip !== currentIP || range !== currentRange) return; // a newer fetch owns the timer
                grid.classList.remove('refreshing');
                if (!dialog.open) return;
                // Raw ranges move every minute; hourly ones every hour, but a
                // printer's live status changes with every poll (Wi-Fi has its own loop)
                const raw = currentRange === '6h' || currentRange === '24h';
                const every = raw || !liveEl.hidden ? 60e3 : 15 * 60e3;
                clearTimeout(refreshTimer); // reopening mid-fetch can leave two requests for the same device
                refreshTimer = setTimeout(() => fetchHistory(), every);
            });
    }

    // ---- live state --------------------------------------------------------
    // Printers show their status from the latest poll. Wi-Fi APs get a fresh
    // read from /api/live every few seconds while the popup is open and the
    // tab is in front. Names come from the network, so all text goes in
    // through textContent.

    // The bar holds the Wi-Fi refresh control. It stays put across redraws so
    // an open dropdown isn't replaced under the pointer.
    const liveBar = el('div', 'live-bar');
    const liveRate = el('select');
    [['0', 'Paused'], ['2', 'Every 2 s'], ['3', 'Every 3 s'], ['5', 'Every 5 s'], ['10', 'Every 10 s']]
        .forEach(([value, text]) => {
            const o = el('option', '', text);
            o.value = value;
            liveRate.appendChild(o);
        });
    liveRate.value = load('snmpLiveRate', '3');
    const liveRateLabel = el('label', 'live-rate', 'Refresh ');
    liveRateLabel.appendChild(liveRate);
    const liveUpdated = el('span', 'live-updated');
    // Disconnecting clients is in Wi-Fi Control, which is admin-only
    const controlLink = el('a', 'live-link', 'Disconnect clients in Wi-Fi Control →');
    controlLink.href = '/wifi/';
    controlLink.hidden = true;
    liveBar.append(liveRateLabel, liveUpdated, controlLink);
    const liveBody = el('div');
    liveEl.append(liveBar, liveBody);
    liveEl.hidden = true;

    function clearLive() {
        stopWifi();
        liveBody.replaceChildren();
        liveEl.hidden = true;
    }

    // The latest poll says what kind of live state a device has; devices that
    // aren't polled return 404
    function fetchLive(ip) {
        fetch(`${base}/api/metrics?ip=${encodeURIComponent(ip)}`)
            .then(res => (res.ok ? res.json() : null))
            .then(m => {
                if (ip !== currentIP) return;
                if (m && m.wifi) startWifi(ip);
                else if (m && m.printer) renderPrinter(m);
                else clearLive();
            })
            .catch(err => console.error('Live metrics fetch failed', err));
    }

    function renderPrinter(m) {
        const at = new Date(m.collected_at).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
        liveBar.hidden = true;
        liveBody.replaceChildren(...printerSections(m.printer, at));
        liveEl.hidden = false;
    }

    // ---- live Wi-Fi --------------------------------------------------------
    // Throughput is the change in a byte counter between two reads; each
    // client and radio keeps about 5 minutes of samples for its sparklines.

    const WIFI_KEEP_MS = 5 * 60e3;
    let wifiIP = '';
    let wifiTimer = null;
    let wifiSeq = 0; // only the newest request's answer is drawn
    let wifiPrev = null; // last snapshot, for rates
    let wifiHist = new Map(); // mac or "radio-N" -> [{t, snr, mbps}]

    function startWifi(ip) {
        if (wifiIP === ip) return;
        stopWifi();
        wifiIP = ip;
        liveBar.hidden = false;
        liveUpdated.textContent = 'Reading the AP…';
        scheduleWifi(0);
    }

    function stopWifi() {
        clearTimeout(wifiTimer);
        wifiSeq++; // a reply still on its way is ignored and schedules nothing
        wifiIP = '';
        wifiPrev = null;
        wifiHist = new Map();
    }

    function scheduleWifi(delay) {
        clearTimeout(wifiTimer);
        const every = Number(liveRate.value) * 1000;
        if (!wifiIP || !dialog.open || document.hidden || !every) return;
        wifiTimer = setTimeout(pollWifi, delay === undefined ? every : delay);
    }

    function pollWifi() {
        const ip = wifiIP, seq = ++wifiSeq;
        fetch(`${base}/api/live?ip=${encodeURIComponent(ip)}`)
            .then(res => res.json().then(body => ({ ok: res.ok, body })))
            .then(({ ok, body }) => {
                if (seq !== wifiSeq || ip !== wifiIP) return;
                if (ok) renderWifi(body);
                else liveUpdated.textContent = body.error || 'Could not read the AP';
            })
            .catch(err => {
                if (seq === wifiSeq) liveUpdated.textContent = 'Could not read the AP';
                console.error('Live Wi-Fi fetch failed', err);
            })
            .finally(() => { if (seq === wifiSeq) scheduleWifi(); });
    }

    // Mbps between two readings of a byte counter; null when there's no
    // earlier reading or the counter went backwards (client reconnected)
    function mbps(prev, cur, secs) {
        if (prev === undefined || cur < prev || secs <= 0) return null;
        return (cur - prev) * 8 / secs / 1e6;
    }

    function remember(key, t, sample) {
        const list = wifiHist.get(key) || [];
        list.push(Object.assign({ t }, sample));
        while (list.length && t - list[0].t > WIFI_KEEP_MS) list.shift();
        wifiHist.set(key, list);
        return list;
    }

    function renderWifi(snap) {
        const t = Date.parse(snap.at);
        // Viewers share reads for a moment, so the same snapshot can come back
        if (wifiPrev && t === wifiPrev.t) return;
        const secs = wifiPrev ? (t - wifiPrev.t) / 1000 : 0;
        const prev = wifiPrev || { radios: new Map(), clients: new Map() };
        const wifi = snap.wifi;

        const radios = liveTable(
            ['Radio', 'Throughput', 'Clients', 'Channel', 'Width', 'Noise', 'Airtime', 'Tx power'],
            wifi.radios.map(r => {
                const p = prev.radios.get(r.index) || {};
                const total = sum(mbps(p.tx, r.tx_bytes, secs), mbps(p.rx, r.rx_bytes, secs));
                const hist = remember(`radio-${r.index}`, t, { mbps: total });
                return [r.band, { node: metric(total === null ? '…' : `${fmtMbps(total)} Mbps`, hist.map(h => h.mbps)) },
                    r.clients, r.channel, `${r.width_mhz} MHz`, `${r.noise_dbm} dBm`, `${r.airtime_pct}%`, `${r.tx_power_dbm} dBm`];
            }));

        const seen = new Set();
        const clients = wifi.clients.slice()
            .sort((a, b) => (a.name || a.ip || a.mac).localeCompare(b.name || b.ip || b.mac));
        const clientTable = liveTable(
            ['Client', 'Signal (SNR)', 'Throughput', 'Rate', 'SSID', 'Band', 'Mode', 'IP', 'MAC', 'Vendor'],
            clients.map(c => {
                seen.add(c.mac);
                const p = prev.clients.get(c.mac) || {};
                const total = sum(mbps(p.tx, c.tx_bytes, secs), mbps(p.rx, c.rx_bytes, secs));
                const hist = remember(c.mac, t, { snr: c.snr_db, mbps: total });
                const weak = c.snr_db < WEAK_SNR_DB;
                return [c.name || '—',
                    { node: metric(`${c.snr_db} dB`, hist.map(h => h.snr), { weak, weakBelow: WEAK_SNR_DB }) },
                    { node: metric(total === null ? '…' : `${fmtMbps(total)} Mbps`, hist.map(h => h.mbps)) },
                    `${formatNum(c.rate_mbps, 0)} Mbps`, c.ssid, c.band, c.mode, c.ip, c.mac, c.vendor];
            }));
        // A client that left starts a fresh history if it comes back
        for (const key of wifiHist.keys()) {
            if (!key.startsWith('radio-') && !seen.has(key)) wifiHist.delete(key);
        }

        liveBody.replaceChildren(
            liveSection('Radios', '', radios),
            liveSection(`Connected clients (${clients.length})`,
                `SNR below ${WEAK_SNR_DB} dB is marked · lines show the last 5 minutes`, clientTable));
        liveUpdated.textContent = `Updated ${new Date(t).toLocaleTimeString()}`;
        controlLink.hidden = !snap.can_control;
        liveEl.hidden = false;

        wifiPrev = {
            t,
            radios: new Map(wifi.radios.map(r => [r.index, { tx: r.tx_bytes, rx: r.rx_bytes }])),
            clients: new Map(wifi.clients.map(c => [c.mac, { tx: c.tx_bytes, rx: c.rx_bytes }])),
        };
    }

    function sum(a, b) {
        return a === null || b === null ? null : a + b;
    }

    function fmtMbps(v) {
        return formatNum(v, v < 10 ? 2 : 1);
    }

    function metric(text, values, opts) {
        const o = opts || {};
        const d = el('div', 'metric');
        d.append(el('span', 'num' + (o.weak ? ' weak' : ''), text), sparkline(values, o.weakBelow));
        return d;
    }

    // A small line of recent values, newest at the right; nulls (no rate yet)
    // break it. It turns red when the latest value is under weakBelow.
    function sparkline(values, weakBelow) {
        const W = 90, H = 22;
        const svg = svgEl('svg', { class: 'spark', viewBox: `0 0 ${W} ${H}`, 'aria-hidden': 'true' });
        const nums = values.filter(v => v !== null && v !== undefined);
        if (nums.length < 2) return svg;
        const hi = Math.max(...nums, weakBelow !== undefined ? weakBelow : 0) || 1;
        const step = W / (values.length - 1);
        let d = '', pen = false;
        values.forEach((v, i) => {
            if (v === null || v === undefined) { pen = false; return; }
            d += `${pen ? 'L' : 'M'}${(i * step).toFixed(1)},${(H - 1 - v / hi * (H - 2)).toFixed(1)}`;
            pen = true;
        });
        const weak = weakBelow !== undefined && nums[nums.length - 1] < weakBelow;
        svg.appendChild(svgEl('path', { d, class: weak ? 'weak' : '' }));
        return svg;
    }

    function printerSections(p, at) {
        const supplies = liveTable(
            ['Supply', 'Remaining', 'Detail'],
            p.supplies.map(s => s.pct !== undefined
                ? [s.name, { text: `${formatNum(s.pct, 0)}%`, cls: s.pct < LOW_SUPPLY_PCT ? 'weak' : '' }, s.note || '']
                : [s.name, { text: s.note, cls: s.note === 'Empty' ? 'weak' : '' }, '']));
        const trays = liveTable(
            ['Tray', 'Capacity', 'Paper'],
            p.trays.map(t => [t.name, t.capacity > 0 ? `${t.capacity} sheets` : '', t.state]));
        const status = [p.display, `${formatNum(p.pages, 0)} pages printed`, `as of ${at}`].filter(Boolean).join(' · ');
        return [
            liveSection(`Status: ${p.status || 'Unknown'}`, status, supplies),
            liveSection('Paper trays', '', trays),
        ];
    }

    function liveSection(title, note, body) {
        const section = el('div', 'live-section');
        const head = el('div', 'live-title', title);
        head.appendChild(el('span', '', note));
        section.append(head, body);
        return section;
    }

    function liveTable(headers, rows) {
        const wrap = el('div', 'live-table-wrap');
        const table = el('table', 'live-table');
        const head = el('tr');
        headers.forEach(h => head.appendChild(el('th', '', h)));
        table.appendChild(head);
        rows.forEach(r => {
            const tr = el('tr');
            r.forEach(v => {
                const cell = v !== null && typeof v === 'object' ? v : { text: v };
                if (cell.node) {
                    const td = el('td', cell.cls || '');
                    td.appendChild(cell.node);
                    tr.appendChild(td);
                    return;
                }
                tr.appendChild(el('td', cell.cls || '', String(cell.text === undefined ? '' : cell.text)));
            });
            table.appendChild(tr);
        });
        wrap.appendChild(table);
        return wrap;
    }

    // Converts a °C point, including an hourly point's max
    function toFahrenheit(p) {
        const f = c => c * 9 / 5 + 32;
        const out = { t: p.t, v: f(p.v) };
        if (p.max !== undefined) out.max = f(p.max);
        if (p.min !== undefined) out.min = f(p.min);
        return out;
    }

    function seriesFrom(map, filter) {
        return Object.keys(map || {}).filter(filter || (() => true)).sort().map(name => ({ name, points: map[name] }));
    }

    function renderAll() {
        const d = lastData;
        // The °F/°C switch only means something when there's a temperature chart
        tempUnitGroup.hidden = !(d && Object.keys(d.temps || {}).length);
        const specs = d ? chartSpecs(d) : [];
        if (!specs.length) {
            specs.push({ key: 'empty', wide: true, empty: d ? 'No data in this range' : 'No history for this device' });
        }
        // Place every card before drawing any, so widths are final when measured
        const placed = specs.map(spec => ({ spec, card: cardFor(spec) }));
        const keep = new Set(placed.map(p => p.card));
        Array.from(grid.children).forEach(c => { if (!keep.has(c)) c.remove(); });
        placed.forEach(({ spec, card }) => {
            if (spec.empty) card.replaceChildren(el('div', 'chart-empty', spec.empty));
            else renderChart(card, spec);
        });
    }

    // Reuses the card for a chart across refreshes; appending also keeps the order
    function cardFor(spec) {
        let card = Array.from(grid.children).find(c => c.dataset.chart === spec.key);
        if (!card) {
            card = el('div', 'chart-card');
            card.dataset.chart = spec.key;
        }
        card.classList.toggle('wide', !!spec.wide);
        grid.appendChild(card);
        return card;
    }

    // One chart per kind of data the device reported in this range
    function chartSpecs(d) {
        const hourly = d.resolution === 'hourly';
        const win = { from: d.from, to: d.to };
        const specs = [];

        if (d.cpu.length) {
            specs.push({
                key: 'cpu', title: 'CPU load', sub: 'Average across all cores, %',
                series: [{ name: 'CPU', points: d.cpu }],
                yFixed: [0, 100], unit: '%', decimals: 1, hourly, win,
            });
        }

        // Stored in °C, as devices report it; shown in the unit picked with the switch
        let temps = seriesFrom(d.temps);
        if (tempUnit === 'F') temps = temps.map(s => ({ name: s.name, points: s.points.map(toFahrenheit) }));
        if (temps.length) {
            specs.push({
                key: 'temp', title: 'Temperature', sub: `°${tempUnit} per sensor`,
                series: temps, nameSeries: true, unit: `°${tempUnit}`, decimals: 1, hourly, win,
            });
        }

        const sensors = d.sensors || {};
        const units = Object.keys(SENSOR_CHARTS).filter(u => sensors[u])
            .concat(Object.keys(sensors).filter(u => !SENSOR_CHARTS[u] && u !== TRAFFIC_UNIT).sort());
        units.forEach(unit => {
            const all = seriesFrom(sensors[unit]);
            if (!all.length) return;
            const label = SENSOR_CHARTS[unit] || { title: unit, sub: '' };
            if (label.rank) {
                const avg = s => s.points.reduce((a, p) => a + p.v, 0) / (s.points.length || 1);
                all.sort((a, b) => label.rank === 'high' ? avg(b) - avg(a) : avg(a) - avg(b));
            }
            const series = all.slice(0, MAX_SERIES).sort((a, b) => a.name.localeCompare(b.name));
            const hidden = all.slice(MAX_SERIES);
            specs.push({
                key: `sensor-${unit}`, title: label.title,
                sub: hidden.length ? `${label.sub} · ${hidden.length} more in the table` : label.sub,
                series, tableOnly: hidden.length ? hidden : undefined,
                nameSeries: true, nonNegative: !label.signed, yFixed: label.yFixed,
                unit: label.unit !== undefined ? label.unit : ` ${unit}`,
                decimals: label.decimals !== undefined ? label.decimals : 2, hourly, win,
            });
        });

        // Router traffic goes first: one chart per interface, in and out
        const ifaces = {};
        Object.keys(sensors[TRAFFIC_UNIT] || {}).forEach(name => {
            const cut = name.lastIndexOf(' ');
            const iface = name.slice(0, cut), dir = name.slice(cut + 1);
            if (!ifaces[iface]) ifaces[iface] = {};
            ifaces[iface][dir] = sensors[TRAFFIC_UNIT][name];
        });
        const traffic = Object.keys(ifaces).sort().map(iface => ({
            key: `traffic-${iface}`, title: iface, sub: 'Traffic, Mbps',
            series: ['in', 'out'].filter(dir => ifaces[iface][dir])
                .map(dir => ({ name: dir === 'in' ? 'In' : 'Out', points: ifaces[iface][dir] })),
            nonNegative: true, unit: ` ${TRAFFIC_UNIT}`, decimals: 2, hourly, win,
        }));
        specs.unshift(...traffic);

        // Chart the real disks, largest first; every filesystem stays in the table
        const all = seriesFrom(d.disks);
        if (all.length) {
            const real = all.filter(s => !VIRTUAL_MOUNT.test(s.name))
                .sort((a, b) => (d.disk_totals[b.name] || 0) - (d.disk_totals[a.name] || 0));
            const disks = real.slice(0, MAX_SERIES).sort((a, b) => a.name.localeCompare(b.name));
            const charted = new Set(disks.map(s => s.name));
            const hidden = all.filter(s => !charted.has(s.name));
            disks.concat(hidden).forEach(s => { s.detail = formatBytes(d.disk_totals[s.name]); });
            specs.push({
                key: 'disk', wide: true, title: 'Disk use',
                sub: hidden.length
                    ? `% used per filesystem · ${hidden.length} small or virtual filesystem${hidden.length > 1 ? 's' : ''} listed in the table only`
                    : '% used per filesystem',
                series: disks, tableOnly: hidden, sizeColumn: true,
                yFixed: [0, 100], unit: '%', decimals: 1, hourly, win,
            });
        }
        return specs;
    }

    // ---- chart -------------------------------------------------------------

    function renderChart(card, opt) {
        card.replaceChildren();
        card.appendChild(el('div', 'chart-title', opt.title));
        card.appendChild(el('div', 'chart-sub', opt.sub));

        const series = opt.series.slice(0, MAX_SERIES);
        series.forEach((s, i) => { s.color = `var(--s${i + 1})`; });
        const named = seriesNamed(series, opt);

        if (!opt.win || series.every(s => s.points.length === 0)) {
            card.appendChild(el('div', 'chart-empty', opt.win ? 'No data in this range' : 'No history for this device'));
            return;
        }

        // A legend whenever series are named; a lone unnamed series is named by the title
        if (named) {
            const legend = el('div', 'chart-legend');
            series.forEach(s => {
                const item = el('span');
                const key = el('span', 'key');
                key.style.background = s.color;
                item.append(key, document.createTextNode(s.name));
                legend.appendChild(item);
            });
            card.appendChild(legend);
        }

        const width = Math.max(card.clientWidth - 32, 280);

        // Direct labels at the end of each line (up to 4 series). The right margin
        // is sized to fit them; when names don't fit, labels show values only
        // (the legend and table still name every series).
        const CHAR_W = 6.3; // approx. width of an 11px character
        let labels = [];
        if (series.length <= 4) {
            labels = series.filter(s => s.points.length).map(s => {
                const last = s.points[s.points.length - 1];
                const value = `${formatNum(last.v, opt.decimals)}${opt.unit}`;
                return { s, v: last.v, value, text: named ? `${value} ${s.name}` : value };
            });
        }
        const labelW = l => 22 + l.length * CHAR_W;
        const maxRight = width * 0.3;
        let right = Math.max(16, ...labels.map(l => labelW(l.text)));
        if (right > maxRight) {
            labels.forEach(l => { l.text = l.value; });
            right = Math.max(16, ...labels.map(l => labelW(l.text)));
        }
        const plotW = width - MARGIN.left - right - 8;
        const plotH = HEIGHT - MARGIN.top - MARGIN.bottom;
        const x0 = opt.win.from, x1 = opt.win.to;
        const xs = t => MARGIN.left + (t - x0) / (x1 - x0) * plotW;

        let [y0, y1] = opt.yFixed || niceExtent(series);
        if (opt.nonNegative) y0 = Math.max(0, y0); // padding shouldn't invent negative traffic or RPM
        const ys = v => MARGIN.top + plotH - (v - y0) / (y1 - y0) * plotH;

        const svg = svgEl('svg', {
            viewBox: `0 0 ${width} ${HEIGHT}`, height: HEIGHT, tabindex: 0, role: 'img',
            'aria-label': `${opt.title} chart; values are in the table below`,
        });

        // Gridlines and y labels
        niceTicks(y0, y1, 5).forEach(v => {
            const y = ys(v);
            svg.appendChild(svgEl('line', { x1: MARGIN.left, x2: MARGIN.left + plotW, y1: y, y2: y,
                stroke: v === y0 ? 'var(--axis)' : 'var(--grid)', 'stroke-width': 1 }));
            const t = svgEl('text', { x: MARGIN.left - 6, y: y + 4, 'text-anchor': 'end' });
            t.textContent = formatNum(v, 0);
            svg.appendChild(t);
        });

        // Time labels
        timeTicks(x0, x1, plotW).forEach(tick => {
            const t = svgEl('text', { x: xs(tick.t), y: HEIGHT - 6, 'text-anchor': 'middle' });
            t.textContent = tick.label;
            svg.appendChild(t);
        });

        // Lines, broken where samples are missing
        const gap = opt.hourly ? 2.5 * 3600e3 : 5 * 60e3;
        series.forEach(s => {
            let d = '', prev = null;
            s.points.forEach(p => {
                d += (prev === null || p.t - prev > gap ? 'M' : 'L') + xs(p.t).toFixed(1) + ',' + ys(p.v).toFixed(1);
                prev = p.t;
            });
            svg.appendChild(svgEl('path', { d, fill: 'none', 'stroke-width': 2,
                'stroke-linejoin': 'round', 'stroke-linecap': 'round', stroke: s.color }));
        });

        // Place the direct labels, spread so they don't overlap
        labels.forEach(l => { l.y = ys(l.v); });
        labels.sort((a, b) => a.y - b.y);
        for (let i = 1; i < labels.length; i++) {
            if (labels[i].y - labels[i - 1].y < 14) labels[i].y = labels[i - 1].y + 14;
        }
        labels.forEach(l => {
            const x = MARGIN.left + plotW + 8;
            svg.appendChild(svgEl('line', { x1: x, x2: x + 10, y1: l.y, y2: l.y,
                'stroke-width': 2, 'stroke-linecap': 'round', stroke: l.s.color }));
            const t = svgEl('text', { x: x + 14, y: l.y + 4, class: 'direct' });
            t.textContent = l.text;
            svg.appendChild(t);
        });

        // Crosshair + tooltip
        const cross = svgEl('line', { y1: MARGIN.top, y2: MARGIN.top + plotH, stroke: 'var(--ink-muted)',
            'stroke-width': 1, visibility: 'hidden' });
        svg.appendChild(cross);
        const dots = series.map(s => {
            const c = svgEl('circle', { r: 4, 'stroke-width': 2, stroke: 'var(--chart-bg)',
                fill: s.color, visibility: 'hidden' });
            svg.appendChild(c);
            return c;
        });
        const hit = svgEl('rect', { x: MARGIN.left, y: MARGIN.top, width: plotW, height: plotH, fill: 'transparent' });
        svg.appendChild(hit);
        card.appendChild(svg);

        const tip = el('div', 'chart-tip');
        tip.hidden = true;
        card.appendChild(tip);

        // Keyboard: arrows step through the timestamps of the longest series
        const steps = series.reduce((a, s) => s.points.length > a.length ? s.points : a, []).map(p => p.t);
        let stepIndex = steps.length - 1;

        function showAt(t) {
            const rows = [];
            let shownT = null;
            series.forEach((s, i) => {
                const p = nearest(s.points, t, gap);
                if (!p) { dots[i].setAttribute('visibility', 'hidden'); return; }
                if (shownT === null) shownT = p.t;
                dots[i].setAttribute('cx', xs(p.t));
                dots[i].setAttribute('cy', ys(p.v));
                dots[i].setAttribute('visibility', 'visible');
                rows.push({ s, p });
            });
            if (shownT === null) { hide(); return; }
            const cx = xs(shownT);
            cross.setAttribute('x1', cx);
            cross.setAttribute('x2', cx);
            cross.setAttribute('visibility', 'visible');

            tip.replaceChildren(el('div', 'tip-time', formatTime(shownT, opt.hourly)));
            rows.forEach(({ s, p }) => {
                const row = el('div', 'tip-row');
                const key = el('span', 'key');
                key.style.background = s.color;
                const fmt = v => `${formatNum(v, opt.decimals)}${opt.unit}`;
                let value = fmt(p.v);
                if (p.min !== undefined || p.max !== undefined) {
                    value += ' avg';
                    if (p.min !== undefined) value += ` · ${fmt(p.min)} min`;
                    if (p.max !== undefined) value += ` · ${fmt(p.max)} max`;
                }
                row.append(key, el('strong', '', value));
                if (named) row.appendChild(document.createTextNode(s.name));
                tip.appendChild(row);
            });
            tip.hidden = false;
            // Place the tooltip beside the crosshair, flipping near the right edge
            const svgBox = svg.getBoundingClientRect(), cardBox = card.getBoundingClientRect();
            const scale = svgBox.width / width;
            const left = svgBox.left - cardBox.left + cx * scale;
            const tipW = tip.offsetWidth;
            const tipLeft = left + 12 + tipW > card.clientWidth ? left - 12 - tipW : left + 12;
            tip.style.left = Math.max(4, Math.min(tipLeft, card.clientWidth - tipW - 4)) + 'px';
            tip.style.top = (svgBox.top - cardBox.top + MARGIN.top * scale) + 'px';
        }
        function hide() {
            cross.setAttribute('visibility', 'hidden');
            dots.forEach(c => c.setAttribute('visibility', 'hidden'));
            tip.hidden = true;
        }
        function pointerT(evt) {
            const r = svg.getBoundingClientRect();
            const px = (evt.clientX - r.left) * (width / r.width);
            return x0 + (px - MARGIN.left) / plotW * (x1 - x0);
        }
        svg.addEventListener('pointermove', e => showAt(pointerT(e)));
        svg.addEventListener('pointerleave', hide);
        svg.addEventListener('focus', () => { if (steps.length) showAt(steps[stepIndex]); });
        svg.addEventListener('blur', hide);
        svg.addEventListener('keydown', e => {
            if (!steps.length || (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight')) return;
            e.preventDefault();
            stepIndex = Math.max(0, Math.min(steps.length - 1, stepIndex + (e.key === 'ArrowRight' ? 1 : -1)));
            showAt(steps[stepIndex]);
        });

        card.appendChild(summaryTable(series, opt));
    }

    // Series carry their own names (legend, labels, table) when there are several,
    // or when the chart asks for it: a lone "450b Retro" port isn't named by "Port current"
    function seriesNamed(series, opt) {
        return series.length > 1 || !!opt.nameSeries;
    }

    // Latest / min / avg / max per series: the chart's values without hovering
    function summaryTable(series, opt) {
        const named = seriesNamed(series, opt);
        const table = el('table', 'chart-table');
        const head = el('tr');
        [named || opt.tableOnly ? 'Series' : '', 'Latest', 'Min', 'Avg', 'Max']
            .forEach(h => head.appendChild(el('th', '', h)));
        if (opt.sizeColumn) head.insertBefore(el('th', '', 'Size'), head.children[1]);
        table.appendChild(head);

        const rows = series.map(s => ({ s, charted: true }))
            .concat((opt.tableOnly || []).map(s => ({ s, charted: false })));
        rows.forEach(({ s, charted }) => {
            if (!s.points.length) return;
            const vals = s.points.map(p => p.v);
            const mins = s.points.map(p => p.min !== undefined ? p.min : p.v);
            const maxes = s.points.map(p => p.max !== undefined ? p.max : p.v);
            const fmt = v => `${formatNum(v, opt.decimals)}${opt.unit}`;
            const tr = el('tr', charted ? '' : 'not-charted');
            const name = el('td');
            if (charted && named) {
                const key = el('span', 'key');
                key.style.background = s.color;
                name.appendChild(key);
            }
            name.appendChild(document.createTextNode(named || opt.tableOnly ? s.name : opt.title));
            tr.appendChild(name);
            if (opt.sizeColumn) tr.appendChild(el('td', '', s.detail || ''));
            [vals[vals.length - 1], Math.min(...mins), vals.reduce((a, b) => a + b, 0) / vals.length, Math.max(...maxes)]
                .forEach(v => tr.appendChild(el('td', '', fmt(v))));
            table.appendChild(tr);
        });
        return table;
    }

    // ---- scales & formatting ----------------------------------------------

    function nearest(points, t, maxDist) {
        let lo = 0, hi = points.length - 1;
        if (hi < 0) return null;
        while (lo < hi) {
            const mid = (lo + hi) >> 1;
            if (points[mid].t < t) lo = mid + 1; else hi = mid;
        }
        let best = points[lo];
        if (lo > 0 && Math.abs(points[lo - 1].t - t) < Math.abs(best.t - t)) best = points[lo - 1];
        return Math.abs(best.t - t) <= maxDist ? best : null;
    }

    function niceExtent(series) {
        let lo = Infinity, hi = -Infinity;
        series.forEach(s => s.points.forEach(p => {
            lo = Math.min(lo, p.min !== undefined ? p.min : p.v);
            hi = Math.max(hi, p.max !== undefined ? p.max : p.v);
        }));
        if (lo === Infinity) return [0, 1];
        const pad = Math.max((hi - lo) * 0.15, 2);
        const step = niceStep((hi - lo + 2 * pad) / 5);
        return [Math.floor((lo - pad) / step) * step, Math.ceil((hi + pad) / step) * step];
    }

    function niceStep(raw) {
        const mag = Math.pow(10, Math.floor(Math.log10(raw)));
        const n = raw / mag;
        return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * mag;
    }

    function niceTicks(lo, hi, count) {
        const step = niceStep((hi - lo) / count);
        const ticks = [];
        for (let v = Math.ceil(lo / step) * step; v <= hi + step / 1e6; v += step) ticks.push(+v.toFixed(6));
        return ticks;
    }

    // Time ticks on local-time boundaries, about one per 90px
    function timeTicks(from, to, plotW) {
        const H = 3600e3, D = 24 * H;
        const steps = [H, 2 * H, 3 * H, 6 * H, 12 * H, D, 2 * D, 7 * D, 14 * D, 30 * D, 61 * D];
        const maxTicks = Math.max(2, Math.floor(plotW / 90));
        const step = steps.find(s => (to - from) / s <= maxTicks) || steps[steps.length - 1];
        const start = new Date(from);
        if (step < D) {
            start.setMinutes(0, 0, 0);
            const stepH = step / H;
            start.setHours(Math.ceil((start.getHours() + (start.getTime() < from ? 1 : 0)) / stepH) * stepH);
        } else {
            start.setHours(0, 0, 0, 0);
            start.setDate(start.getDate() + 1);
        }
        const ticks = [];
        // Step on the calendar so ticks stay on midnight / whole hours across DST changes
        for (const t = new Date(start); t.getTime() <= to; ) {
            if (t.getTime() >= from) {
                ticks.push({
                    t: t.getTime(),
                    label: step < D
                        ? (t.getHours() === 0 ? t.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
                            : t.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' }))
                        : t.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
                });
            }
            if (step < D) t.setHours(t.getHours() + step / H);
            else t.setDate(t.getDate() + step / D);
        }
        return ticks;
    }

    function formatTime(t, hourly) {
        const d = new Date(t);
        const opts = { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' };
        return hourly ? `${d.toLocaleString(undefined, opts)} (hour)` : d.toLocaleString(undefined, opts);
    }

    function formatNum(v, decimals) {
        return Number(v).toLocaleString(undefined, { minimumFractionDigits: decimals, maximumFractionDigits: decimals });
    }

    function formatBytes(b) {
        if (!b) return '';
        const units = ['B', 'KB', 'MB', 'GB', 'TB'];
        let i = 0;
        while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
        return `${b.toFixed(b < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
    }

    // ---- controls ----------------------------------------------------------

    function markRange() {
        rangeGroup.querySelectorAll('button').forEach(b => {
            b.setAttribute('aria-pressed', b.dataset.range === currentRange ? 'true' : 'false');
        });
    }

    rangeGroup.addEventListener('click', e => {
        const b = e.target.closest('button[data-range]');
        if (!b || b.dataset.range === currentRange) return;
        currentRange = b.dataset.range;
        save('snmpHistoryRange', currentRange);
        markRange();
        fetchHistory();
    });

    function markTempUnit() {
        tempUnitGroup.querySelectorAll('button').forEach(b => {
            b.setAttribute('aria-pressed', b.dataset.unit === tempUnit ? 'true' : 'false');
        });
    }

    // Switching units redraws from the data already loaded
    tempUnitGroup.addEventListener('click', e => {
        const b = e.target.closest('button[data-unit]');
        if (!b || b.dataset.unit === tempUnit) return;
        tempUnit = b.dataset.unit;
        save('snmpTempUnit', tempUnit);
        markTempUnit();
        if (lastData) renderAll();
    });

    document.getElementById('historyClose').addEventListener('click', () => dialog.close());
    // Content sits in .dialog-body, so a click on the dialog itself is the backdrop
    dialog.addEventListener('click', e => { if (e.target === dialog) dialog.close(); });
    dialog.addEventListener('close', () => {
        clearTimeout(refreshTimer);
        stopWifi(); // reopening starts a fresh live view
    });

    liveRate.addEventListener('change', () => {
        save('snmpLiveRate', liveRate.value);
        wifiPrev = null; // a paused gap shouldn't count as one slow sample
        scheduleWifi(0);
    });
    // Stop reading the AP while the tab is in the background
    document.addEventListener('visibilitychange', () => {
        if (document.hidden) {
            clearTimeout(wifiTimer);
        } else if (wifiIP) {
            wifiPrev = null;
            scheduleWifi(0);
        }
    });

    let resizeTimer = null;
    window.addEventListener('resize', () => {
        clearTimeout(resizeTimer);
        resizeTimer = setTimeout(() => { if (lastData && dialog.open) renderAll(); }, 150);
    });

    // Colours are CSS variables, so the theme toggle restyles the charts without a re-render
    markRange();
    markTempUnit();
})();
