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
    const statusEl = document.getElementById('historyStatus');
    const grid = document.getElementById('historyCharts');
    // Titles for sensor units; any other unit is charted under its own name
    const SENSOR_CHARTS = {
        V: { title: 'Voltage', sub: 'Volts per input' },
        A: { title: 'Port current', sub: 'Amps drawn per power port' },
    };

    let currentIP = '';
    let currentRange = load('snmpHistoryRange', '24h');
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
                // Raw ranges move every minute; hourly ones every hour
                const every = currentRange === '6h' || currentRange === '24h' ? 60e3 : 15 * 60e3;
                clearTimeout(refreshTimer); // reopening mid-fetch can leave two requests for the same device
                refreshTimer = setTimeout(() => fetchHistory(), every);
            });
    }

    function seriesFrom(map, filter) {
        return Object.keys(map || {}).filter(filter || (() => true)).sort().map(name => ({ name, points: map[name] }));
    }

    function renderAll() {
        const d = lastData;
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

        const temps = seriesFrom(d.temps);
        if (temps.length) {
            specs.push({
                key: 'temp', title: 'Temperature', sub: '°C per sensor',
                series: temps, unit: '°C', decimals: 1, hourly, win,
            });
        }

        const sensors = d.sensors || {};
        Object.keys(sensors).sort().forEach(unit => {
            const series = seriesFrom(sensors[unit]);
            if (!series.length) return;
            const label = SENSOR_CHARTS[unit] || { title: unit, sub: '' };
            specs.push({
                key: `sensor-${unit}`, title: label.title, sub: label.sub,
                series, unit: ` ${unit}`, decimals: 2, hourly, win,
            });
        });

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
                series: disks, tableOnly: hidden,
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

        if (!opt.win || series.every(s => s.points.length === 0)) {
            card.appendChild(el('div', 'chart-empty', opt.win ? 'No data in this range' : 'No history for this device'));
            return;
        }

        // A legend whenever there is more than one series; one series is named by the title
        if (series.length > 1) {
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
                return { s, v: last.v, value, text: series.length > 1 ? `${value} ${s.name}` : value };
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
                if (series.length > 1) row.appendChild(document.createTextNode(s.name));
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

    // Latest / min / avg / max per series: the chart's values without hovering
    function summaryTable(series, opt) {
        const table = el('table', 'chart-table');
        const head = el('tr');
        [series.length > 1 || opt.tableOnly ? 'Series' : '', 'Latest', 'Min', 'Avg', 'Max']
            .forEach(h => head.appendChild(el('th', '', h)));
        if (opt.tableOnly) head.insertBefore(el('th', '', 'Size'), head.children[1]);
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
            if (charted && series.length > 1) {
                const key = el('span', 'key');
                key.style.background = s.color;
                name.appendChild(key);
            }
            name.appendChild(document.createTextNode(series.length > 1 || opt.tableOnly ? s.name : opt.title));
            tr.appendChild(name);
            if (opt.tableOnly) tr.appendChild(el('td', '', s.detail || ''));
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

    document.getElementById('historyClose').addEventListener('click', () => dialog.close());
    // Content sits in .dialog-body, so a click on the dialog itself is the backdrop
    dialog.addEventListener('click', e => { if (e.target === dialog) dialog.close(); });
    dialog.addEventListener('close', () => clearTimeout(refreshTimer));

    let resizeTimer = null;
    window.addEventListener('resize', () => {
        clearTimeout(resizeTimer);
        resizeTimer = setTimeout(() => { if (lastData && dialog.open) renderAll(); }, 150);
    });

    // Colours are CSS variables, so the theme toggle restyles the charts without a re-render
    markRange();
})();
