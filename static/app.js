let devices = [];
let nextScanAt = null;
let polledIPs = new Set(); // devices with metrics, from /api/metrics

let basePath = '';
if (window.location.pathname.includes('/snmp')) {
    basePath = '/snmp';
}

// Load devices and scheduler state on page load
loadDevices();
fetch(`${basePath}/api/scan-status`)
    .then(res => res.json())
    .then(status => {
        if (status.next_scan && status.next_scan !== '0001-01-01T00:00:00Z') {
            nextScanAt = new Date(status.next_scan);
            updateNextScanCountdown();
        }
    });
setInterval(loadDevices, 5000);
setInterval(updateNextScanCountdown, 1000);

function loadDevices() {
    Promise.all([
        fetch(`${basePath}/api/devices`).then(res => res.json()),
        fetch(`${basePath}/api/metrics`).then(res => res.json()).catch(() => ({})),
    ]).then(([data, metrics]) => {
        devices = data.devices || [];
        polledIPs = new Set((metrics.devices || []).map(m => m.ip));
        updateStats(data);
        renderDeviceTable();
        renderDeviceTree();
    });
}

// Only touch the DOM when the markup changed, so the 5s refresh doesn't drop
// keyboard focus or re-expand collapsed subnets
const lastHTML = {};
function setHTML(el, html) {
    if (lastHTML[el.id] === html) return;
    lastHTML[el.id] = html;
    el.innerHTML = html;
}

function statsBadge(ip) {
    return polledIPs.has(ip) ? '<span class="stats-badge">📈 Stats</span>' : '';
}

// Clicking (or Enter/Space on) a device opens its history popup (history.js)
function onDevicePick(e) {
    if (e.type === 'keydown' && e.key !== 'Enter' && e.key !== ' ') return;
    const item = e.target.closest('[data-ip]');
    if (!item || !window.openDeviceHistory) return;
    e.preventDefault();
    const ip = item.dataset.ip;
    const device = devices.find(d => (d.ip || d.IP) === ip) || {};
    window.openDeviceHistory(ip, device.hostname || device.Hostname || '');
}
['deviceTableContainer', 'deviceTree'].forEach(id => {
    const container = document.getElementById(id);
    container.addEventListener('click', onDevicePick);
    container.addEventListener('keydown', onDevicePick);
});

function renderDeviceTable() {
    const container = document.getElementById('deviceTableContainer');
    if (!devices || devices.length === 0) {
        setHTML(container, '<div class="empty-state"><h3>No devices found</h3></div>');
        return;
    }
    let html = '<table class="device-table">';
    html += '<thead><tr>'
        + '<th>IP</th>'
        + '<th>Hostname</th>'
        + '<th>Description</th>'
        + '<th>Location</th>'
        + '<th>Community</th>'
        + '</tr></thead><tbody>';
    devices.forEach(device => {
        const ip = device.ip || device.IP;
        if (!device || typeof ip !== 'string' || !ip.match(/^\d+\.\d+\.\d+\.\d+$/)) return;
        html += `<tr data-ip="${ip}" tabindex="0" title="Show stats for ${ip}">`
            + `<td class="ip-cell">${ip}${statsBadge(ip)}</td>`
            + `<td>${device.hostname || device.Hostname || ''}</td>`
            + `<td>${device.description || device.Description || ''}</td>`
            + `<td>${device.location || device.Location || ''}</td>`
            + `<td>${device.community || device.Community || ''}</td>`
            + '</tr>';
    });
    html += '</tbody></table>';
    setHTML(container, html);
}

function updateStats(data) {
    document.getElementById('deviceCount').textContent = data.device_count || 0;

    if (data.last_scan && data.last_scan !== '0001-01-01T00:00:00Z') {
        const lastScan = new Date(data.last_scan);
        document.getElementById('lastScan').textContent = lastScan.toLocaleTimeString();
    }
}

function updateNextScanCountdown() {
    const el = document.getElementById('nextScan');
    if (!nextScanAt || nextScanAt <= new Date()) {
        return;
    }
    const secsLeft = Math.round((nextScanAt - new Date()) / 1000);
    if (secsLeft <= 0) {
        el.textContent = 'Soon';
        return;
    }
    const h = Math.floor(secsLeft / 3600);
    const m = Math.floor((secsLeft % 3600) / 60);
    const s = secsLeft % 60;
    if (h > 0) {
        el.textContent = `${h}h ${m}m`;
    } else if (m > 0) {
        el.textContent = `${m}m ${s}s`;
    } else {
        el.textContent = `${s}s`;
    }
}

function renderDeviceTree() {
    const tree = document.getElementById('deviceTree');
    
    if (devices.length === 0) {
        setHTML(tree, '<div class="empty-state"><h3>No devices found</h3><p>Click "Scan Network" to discover devices</p></div>');
        return;
    }

    // Group devices by subnet (first 3 octets)
    const subnets = {};
    devices.forEach(device => {
        const ip = device.ip || device.IP;
        if (!device || typeof ip !== 'string' || !ip.match(/^\d+\.\d+\.\d+\.\d+$/)) {
            return; // skip invalid device
        }
        const subnet = ip.split('.').slice(0, 3).join('.') + '.0/24';
        if (!subnets[subnet]) {
            subnets[subnet] = [];
        }
        subnets[subnet].push(device);
    });

    // Render tree
    let html = '';
    Object.keys(subnets).sort().forEach(subnet => {
        const devicesInSubnet = subnets[subnet];
        html += `
            <div class="subnet-group">
                <div class="subnet-header" onclick="toggleSubnet(this)">
                    <span>📡 ${subnet}</span>
                    <span class="count">${devicesInSubnet.length}</span>
                </div>
                <div class="device-list">
                    ${devicesInSubnet.map(device => {
                        const ip = device.ip || device.IP;
                        const hostname = device.hostname || device.Hostname;
                        const description = device.description || device.Description;
                        const latency = device.latency || device.Latency;
                        const location = device.location || device.Location;
                        const community = device.community || device.Community;
                        return `
                        <div class="device-item" data-ip="${ip}" tabindex="0" title="Show stats for ${ip}">
                            <div class="device-ip">${ip}${statsBadge(ip)}</div>
                            ${hostname ? `<div class="device-hostname">🏷️ ${hostname}</div>` : ''}
                            ${description ? `<div class="device-description">${description}</div>` : ''}
                            <div class="device-meta">
                                ${latency ? `<span class="device-meta-item">⚡ ${latency}</span>` : ''}
                                ${location ? `<span class="device-meta-item">📍 ${location}</span>` : ''}
                                ${community ? `<span class="device-meta-item">🔑 ${community}</span>` : ''}
                            </div>
                        </div>
                        `;
                    }).join('')}
                </div>
            </div>
        `;
    });

    setHTML(tree, html);
}

function toggleSubnet(header) {
    const deviceList = header.nextElementSibling;
    if (deviceList.style.display === 'none') {
        deviceList.style.display = 'block';
    } else {
        deviceList.style.display = 'none';
    }
}

function startScan() {
    const btn = document.getElementById('scanBtn');
    const loading = document.getElementById('loading');
    const progressBarContainer = document.getElementById('progressBarContainer');
    const progressBar = document.getElementById('progressBar');
    const progressText = document.getElementById('progressText');
    const loadingText = document.getElementById('loadingText');

    btn.disabled = true;
    btn.textContent = 'Scanning...';
    loading.classList.add('active');
    progressBarContainer.style.display = 'block';
    progressBar.style.width = '0%';
    progressText.textContent = '';
    loadingText.textContent = 'Scanning network...';

    let pollInterval;
    function pollScanStatus() {
        fetch(`${basePath}/api/scan-status`)
            .then(res => res.json())
            .then(status => {
                if (status.next_scan && status.next_scan !== '0001-01-01T00:00:00Z') {
                    nextScanAt = new Date(status.next_scan);
                    updateNextScanCountdown();
                }
                if (status && status.running) {
                    let percent = status.total > 0 ? Math.round((status.progress / status.total) * 100) : 0;
                    progressBar.style.width = percent + '%';
                    progressText.textContent = status.message || '';
                    loadingText.textContent = 'Scanning network...';
                } else {
                    progressBar.style.width = '100%';
                    progressText.textContent = status.message || 'Scan complete';
                    loadingText.textContent = '';
                    clearInterval(pollInterval);
                    setTimeout(() => {
                        btn.disabled = false;
                        btn.textContent = 'Scan Network';
                        loading.classList.remove('active');
                        progressBarContainer.style.display = 'none';
                        loadDevices();
                    }, 1200);
                }
            })
            .catch(err => {
                console.error('Error polling scan status:', err);
            });
    }

    fetch(`${basePath}/api/scan`, { method: 'POST' })
        .then(res => res.json())
        .then(data => {
            pollInterval = setInterval(pollScanStatus, 400);
        });
}
