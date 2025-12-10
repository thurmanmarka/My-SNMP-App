let devices = [];

let basePath = '';
if (window.location.pathname.includes('/snmp')) {
    basePath = '/snmp';
}

// Load devices on page load
loadDevices();
setInterval(loadDevices, 5000); // Refresh every 5 seconds

function loadDevices() {
    fetch(`${basePath}/api/devices`)
        .then(res => res.json())
        .then(data => {
            devices = data.devices || [];
            updateStats(data);
            renderDeviceTable();
            renderDeviceTree();
        });
}

function renderDeviceTable() {
    const container = document.getElementById('deviceTableContainer');
    if (!devices || devices.length === 0) {
        container.innerHTML = '<div class="empty-state"><h3>No devices found</h3></div>';
        return;
    }
    let html = '<table style="width:100%;border-collapse:collapse;background:white;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,0.04);overflow:hidden;">';
    html += '<thead><tr style="background:#f5f5f5;color:#333;font-size:15px;">'
        + '<th style="padding:10px 8px;text-align:left;">IP</th>'
        + '<th style="padding:10px 8px;text-align:left;">Hostname</th>'
        + '<th style="padding:10px 8px;text-align:left;">Description</th>'
        + '<th style="padding:10px 8px;text-align:left;">Location</th>'
        + '<th style="padding:10px 8px;text-align:left;">Community</th>'
        + '</tr></thead><tbody>';
    devices.forEach(device => {
        const ip = device.ip || device.IP;
        if (!device || typeof ip !== 'string' || !ip.match(/^\d+\.\d+\.\d+\.\d+$/)) return;
        html += '<tr style="border-bottom:1px solid #eee;">'
            + `<td style="padding:8px 8px;color:#667eea;font-weight:bold;">${ip}</td>`
            + `<td style="padding:8px 8px;">${device.hostname || device.Hostname || ''}</td>`
            + `<td style="padding:8px 8px;">${device.description || device.Description || ''}</td>`
            + `<td style="padding:8px 8px;">${device.location || device.Location || ''}</td>`
            + `<td style="padding:8px 8px;">${device.community || device.Community || ''}</td>`
            + '</tr>';
    });
    html += '</tbody></table>';
    container.innerHTML = html;
}

function updateStats(data) {
    document.getElementById('deviceCount').textContent = data.device_count || 0;
    
    if (data.last_scan && data.last_scan !== '0001-01-01T00:00:00Z') {
        const lastScan = new Date(data.last_scan);
        document.getElementById('lastScan').textContent = lastScan.toLocaleTimeString();
    }
}

function renderDeviceTree() {
    const tree = document.getElementById('deviceTree');
    
    if (devices.length === 0) {
        tree.innerHTML = '<div class="empty-state"><h3>No devices found</h3><p>Click "Scan Network" to discover devices</p></div>';
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
                        <div class="device-item">
                            <div class="device-ip">${ip}</div>
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

    tree.innerHTML = html;
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
                console.log('Scan status:', status);
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
