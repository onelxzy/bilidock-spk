'use strict';

const $ = (id) => document.getElementById(id);

const state = {
  status: null,
  parsed: null,
  streams: { videos: [], audios: [], defaults: {} },
  activeEpId: 0,
  selectedIds: new Set(), // 番剧保存 ep_id，多 P 视频保存 page 编号
  tasks: [],
  filter: 'all', // all | active | done
  qrTimer: null,
  cooldown: 0,
  defaultUA: '',
};

/* ---------- 基础工具 ---------- */
function toast(text, isErr) {
  const el = $('toast');
  el.textContent = text;
  el.className = 'toast' + (isErr ? ' err' : '');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => el.classList.add('hidden'), 3200);
}

function cgiBase() {
  const p = location.pathname;
  const i = p.indexOf('.cgi');
  if (i >= 0) return p.slice(0, i + 4);
  if (p.endsWith('/')) return p;
  return p + '/';
}

function apiURL(path) {
  const clean = '/' + String(path).replace(/^\/+/, '');
  return cgiBase() + '?__api=' + encodeURIComponent(clean);
}

async function api(path, opts) {
  const res = await fetch(
    apiURL(path),
    Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts || {})
  );
  let data = null;
  try {
    data = await res.json();
  } catch (_) {
    /* ignore */
  }
  if (!res.ok || (data && data.error)) {
    throw new Error((data && data.error) || 'HTTP ' + res.status);
  }
  return data;
}

function imgURL(u) {
  if (!u) return '';
  return apiURL('api/cover?url=' + encodeURIComponent(u));
}

function fmtBytes(n) {
  if (!n || n < 0) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return (i === 0 ? n.toFixed(0) : n.toFixed(n >= 100 ? 0 : 1)) + ' ' + u[i];
}

function fmtSpeed(n) {
  return n > 0 ? fmtBytes(n) + '/s' : '';
}

function fmtDuration(sec) {
  if (!sec || sec <= 0) return '';
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.floor(sec % 60);
  const pad = (x) => String(x).padStart(2, '0');
  return h > 0 ? h + ':' + pad(m) + ':' + pad(s) : pad(m) + ':' + pad(s);
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])
  );
}

/* ---------- 顶部状态 ---------- */
async function loadStatus() {
  try {
    const st = await api('api/status');
    state.status = st;
    if (st.version) $('verText').textContent = 'v' + st.version;

    const login = st.login || {};
    const pill = $('accountPill');
    const avatar = $('userAvatar');
    const vip = $('vipBadge');

    if (login.logged_in) {
      pill.className = 'account-pill is-ok';
      $('statusText').textContent = login.name || '已登录';
      if (login.face) {
        avatar.src = imgURL(login.face);
        avatar.classList.remove('hidden');
      } else {
        avatar.classList.add('hidden');
      }
      if (login.vip) {
        vip.textContent = login.vip_label || '大会员';
        vip.classList.remove('hidden');
      } else {
        vip.classList.add('hidden');
      }
      $('btnLogin').textContent = '切换账号';
    } else {
      pill.className = 'account-pill is-warn';
      avatar.classList.add('hidden');
      vip.classList.add('hidden');
      $('statusText').textContent = '未登录 · 仅低画质';
      $('btnLogin').textContent = '扫码登录';
    }

    setCooldown(st.cooldown || 0);
    setDirState(st);
  } catch (e) {
    $('accountPill').className = 'account-pill is-err';
    $('statusText').textContent = '服务异常';
  }
}

function setDirState(st) {
  const dir = (st && st.download_dir) || '';
  const base = dir ? dir.split(/[\\/]/).filter(Boolean).pop() || dir : '未设置';
  $('dirLabel').textContent = st && st.download_dir_writable === false ? base + ' (不可写)' : base;
  $('btnDirPill').className =
    'chip-btn' + (st && st.download_dir_writable === false ? ' is-bad' : '');
  $('footDir').textContent = dir ? '保存路径：' + dir : '';

  const banner = $('dirBanner');
  if (!st || st.download_dir_writable !== false) {
    banner.classList.add('hidden');
    return;
  }
  banner.classList.remove('hidden');
  $('dirText').textContent =
    '当前目录 ' + (dir || '(未设置)') + ' 不可写，请改到有读写权限的共享文件夹。';
}

function setCooldown(sec) {
  state.cooldown = sec || 0;
  const banner = $('riskBanner');
  if (state.cooldown > 0) {
    banner.classList.remove('hidden');
    $('riskText').textContent =
      '触发风控冷却，剩余 ' + state.cooldown + ' 秒，期间不会发起新的 B 站请求。';
  } else {
    banner.classList.add('hidden');
  }
}

/* ---------- 登录 ---------- */
async function openLogin() {
  $('loginModal').classList.remove('hidden');
  $('qrBox').innerHTML = '';
  $('qrStatus').textContent = '正在获取二维码…';
  await refreshQR();
}

async function refreshQR() {
  if (state.qrTimer) {
    clearInterval(state.qrTimer);
    state.qrTimer = null;
  }
  $('qrStatus').textContent = '正在获取二维码…';
  try {
    const qr = await api('api/login/qr');
    $('qrBox').innerHTML = '<img alt="二维码" src="' + qr.png + '">';
    $('qrStatus').textContent = '等待手机扫码…';
    state.qrTimer = setInterval(async () => {
      try {
        const r = await api('api/login/poll?key=' + encodeURIComponent(qr.key));
        if (r.code === 0) {
          clearInterval(state.qrTimer);
          state.qrTimer = null;
          $('qrStatus').textContent = '登录成功';
          toast('登录成功');
          $('loginModal').classList.add('hidden');
          loadStatus();
          return;
        }
        const map = {
          86101: '等待手机扫码…',
          86090: '已扫码，请在手机上点击确认',
          86038: '二维码已过期，请点击刷新',
        };
        $('qrStatus').textContent = map[r.code] || r.message || '等待中…';
        if (r.code === 86038 && state.qrTimer) {
          clearInterval(state.qrTimer);
          state.qrTimer = null;
        }
      } catch (_) {
        /* 保持轮询 */
      }
    }, 2000);
  } catch (e) {
    $('qrStatus').textContent = '获取二维码失败：' + e.message;
  }
}

async function logout() {
  try {
    await api('api/logout', { method: 'POST', body: '{}' });
    toast('已退出登录');
    loadStatus();
  } catch (e) {
    toast(e.message, true);
  }
}

/* ---------- 解析与选集 ---------- */
function optionLabel(v) {
  if (v.width) {
    return v.label + ' · ' + v.width + '×' + v.height;
  }
  return v.label + (v.bandwidth ? ' · ' + Math.round(v.bandwidth / 1000) + ' kbps' : '');
}

async function doParse() {
  const input = $('input').value.trim();
  if (!input) {
    toast('请先粘贴链接或输入 BV / ep / ss 号', true);
    $('input').focus();
    return;
  }
  const btn = $('btnParse');
  btn.disabled = true;
  btn.textContent = '解析中…';
  try {
    const r = await api('api/parse', { method: 'POST', body: JSON.stringify({ input }) });
    state.parsed = r;
    renderResult(r);
    $('resultCard').classList.remove('hidden');
  } catch (e) {
    toast('解析失败：' + e.message, true);
  } finally {
    btn.disabled = false;
    btn.textContent = '解析';
  }
}

function closeResult() {
  $('resultCard').classList.add('hidden');
  state.parsed = null;
}

function renderResult(r) {
  state.selectedIds = new Set();
  state.activeEpId = 0;

  if (r.kind === 'pgc') {
    renderPgc(r);
    return;
  }

  const v = r.video || {};
  $('rKindBadge').textContent = v.pages && v.pages.length > 1 ? '多P合集' : '视频';
  $('rKindBadge').className = 'kind-badge';
  $('rCover').src = imgURL(v.cover);
  $('rCover').alt = v.title || '';
  $('rTitle').textContent = v.title || '';

  const dur = fmtDuration(v.duration);
  if (dur) {
    $('rDuration').textContent = dur;
    $('rDuration').classList.remove('hidden');
  } else {
    $('rDuration').classList.add('hidden');
  }

  const bits = [];
  if (v.owner) bits.push('UP：' + v.owner);
  if (v.bvid) bits.push(v.bvid);
  if (v.pages && v.pages.length > 1) bits.push('共 ' + v.pages.length + ' P');
  $('rMeta').textContent = bits.join(' · ');

  $('streamNotice').classList.add('hidden');
  fillStreams(r);

  const pages = v.pages || [];
  if (pages.length > 1) {
    const defPage = r.page || 1;
    state.selectedIds.add(defPage);
    renderChecklist(
      pages.map((p) => ({
        id: p.page,
        tag: 'P' + p.page,
        name: p.part || '分 P ' + p.page,
        badge: '',
      })),
      defPage,
      '选择分 P（共 ' + pages.length + ' 个）',
      null
    );
  } else {
    $('itemsBox').classList.add('hidden');
    updateDownloadBtn();
  }
}

function renderPgc(r) {
  const p = r.pgc || {};
  $('rKindBadge').textContent = p.type_name || '番剧';
  $('rKindBadge').className = 'kind-badge is-pgc';
  $('rCover').src = imgURL(p.cover || p.square_cover);
  $('rCover').alt = p.title || '';
  $('rTitle').textContent = p.title || '';
  $('rDuration').classList.add('hidden');

  const bits = [];
  if (p.season_title && p.season_title !== p.title) bits.push(p.season_title);
  if (p.total) bits.push('正片 ' + p.total + ' 集');
  if (p.season_id) bits.push('ss' + p.season_id);
  $('rMeta').textContent = bits.join(' · ');

  if (r.stream_notice) {
    $('streamNotice').textContent = r.stream_notice;
    $('streamNotice').classList.remove('hidden');
  } else {
    $('streamNotice').classList.add('hidden');
  }

  fillStreams(r);

  const eps = r.episodes || [];
  const defEp = r.default_ep || (eps[0] && eps[0].ep_id) || 0;
  if (defEp) state.selectedIds.add(defEp);
  state.activeEpId = defEp;

  renderChecklist(
    eps.map((e) => ({
      id: e.ep_id,
      tag: e.title ? String(e.title) : 'ep' + e.ep_id,
      name: e.long_title || e.title || 'ep' + e.ep_id,
      badge: e.badge || '',
    })),
    defEp,
    '选集（共 ' + eps.length + ' 项）',
    async (epId) => {
      state.activeEpId = epId;
      highlightChecklistRow(epId);
      try {
        const res = await api('api/ep/streams', {
          method: 'POST',
          body: JSON.stringify({
            input: (state.parsed && state.parsed.resolved_input) || $('input').value.trim(),
            ep_id: epId,
          }),
        });
        $('streamNotice').classList.add('hidden');
        fillStreams(res);
      } catch (e) {
        if (/试看|大会员|购买/.test(e.message)) {
          $('streamNotice').textContent = e.message;
          $('streamNotice').classList.remove('hidden');
          $('selQn').innerHTML = '<option value="0">该集需要大会员或购买</option>';
          $('selAudio').innerHTML = '<option value="0">—</option>';
        } else {
          toast('读取剧集画质失败：' + e.message, true);
        }
      }
    }
  );
}

function renderChecklist(items, activeId, labelText, onPreview) {
  const box = $('itemsBox');
  const list = $('itemsList');
  $('itemsLabel').textContent = labelText;
  box.classList.remove('hidden');

  list.innerHTML = items
    .map((it) => {
      const checked = state.selectedIds.has(it.id) ? 'checked' : '';
      const active = it.id === activeId ? ' is-active' : '';
      const badge = it.badge ? '<span class="ep-badge">' + esc(it.badge) + '</span>' : '';
      return (
        '<div class="ep-row' +
        active +
        '" data-id="' +
        it.id +
        '">' +
        '<input type="checkbox" ' +
        checked +
        '>' +
        '<span class="ep-num">' +
        esc(it.tag) +
        '</span>' +
        '<span class="ep-name" title="' +
        esc(it.name) +
        '">' +
        esc(it.name) +
        '</span>' +
        badge +
        '</div>'
      );
    })
    .join('');

  list.querySelectorAll('.ep-row').forEach((row) => {
    const id = Number(row.dataset.id);
    const cb = row.querySelector('input[type="checkbox"]');
    cb.onclick = (ev) => {
      ev.stopPropagation();
      if (cb.checked) state.selectedIds.add(id);
      else state.selectedIds.delete(id);
      updateDownloadBtn();
    };
    row.onclick = () => {
      if (onPreview) {
        onPreview(id);
      } else {
        cb.checked = !cb.checked;
        if (cb.checked) state.selectedIds.add(id);
        else state.selectedIds.delete(id);
        updateDownloadBtn();
      }
    };
  });

  $('btnSelAll').onclick = () => {
    items.forEach((it) => state.selectedIds.add(it.id));
    list.querySelectorAll('input[type="checkbox"]').forEach((cb) => (cb.checked = true));
    updateDownloadBtn();
  };
  $('btnSelNone').onclick = () => {
    state.selectedIds.clear();
    list.querySelectorAll('input[type="checkbox"]').forEach((cb) => (cb.checked = false));
    updateDownloadBtn();
  };
  updateDownloadBtn();
}

function highlightChecklistRow(id) {
  $('itemsList')
    .querySelectorAll('.ep-row')
    .forEach((row) => {
      row.classList.toggle('is-active', Number(row.dataset.id) === id);
    });
}

function updateDownloadBtn() {
  const btn = $('btnDownload');
  const r = state.parsed;
  if (!r) return;
  const isMulti = r.kind === 'pgc' || (r.video && r.video.pages && r.video.pages.length > 1);
  if (!isMulti) {
    btn.textContent = '加入下载队列';
    btn.disabled = false;
    return;
  }
  const n = state.selectedIds.size;
  if (n <= 0) {
    btn.textContent = '请至少勾选 1 项';
    btn.disabled = true;
  } else if (n === 1) {
    btn.textContent = '加入下载队列 (已选 1 项)';
    btn.disabled = false;
  } else {
    btn.textContent = '批量加入下载队列 (' + n + ' 项)';
    btn.disabled = false;
  }
}

function renderVideoOptions(keepQn) {
  const selQn = $('selQn');
  const codec = Number($('selCodec').value || 0);
  const allVideos = state.streams.videos || [];

  // 若选定编码有可用流则只展示该编码对应的画质；否则回退展示全部可用画质
  let filtered = codec > 0 ? allVideos.filter((v) => v.codecid === codec) : allVideos;
  if (!filtered.length) filtered = allVideos;

  // 对同一画质 ID 去重，优先保留高码率
  const byQn = new Map();
  filtered.forEach((v) => {
    const old = byQn.get(v.id);
    if (!old || (v.bandwidth || 0) > (old.bandwidth || 0)) {
      byQn.set(v.id, v);
    }
  });
  const list = Array.from(byQn.values()).sort((a, b) => b.id - a.id);

  selQn.innerHTML = '';
  list.forEach((s) => {
    const o = document.createElement('option');
    o.value = s.id;
    o.textContent = optionLabel(s);
    selQn.appendChild(o);
  });
  if (!selQn.options.length) {
    const o = document.createElement('option');
    o.value = '0';
    o.textContent = '无可用画质';
    selQn.appendChild(o);
    return;
  }

  const wantQn = keepQn || (state.streams.defaults && state.streams.defaults.qn);
  if (wantQn) {
    for (const o of selQn.options) {
      if (Number(o.value) > 0 && Number(o.value) <= wantQn) {
        selQn.value = o.value;
        break;
      }
    }
  }
}

function fillStreams(r) {
  state.streams = {
    videos: r.videos || [],
    audios: r.audios || [],
    defaults: r.defaults || {},
  };

  const selCodec = $('selCodec');
  const allVideos = state.streams.videos;
  const availCodecs = new Set(allVideos.map((v) => v.codecid).filter(Boolean));
  for (const o of selCodec.options) {
    const cid = Number(o.value);
    if (cid === 0) {
      o.disabled = false;
      o.textContent = '自动优先';
    } else {
      const has = availCodecs.has(cid);
      o.disabled = allVideos.length > 0 && !has;
      const base = cid === 7 ? 'AVC (H.264)' : cid === 12 ? 'HEVC (H.265)' : 'AV1';
      o.textContent = has || !allVideos.length ? base : base + ' · 无';
    }
  }
  const wantCodec = r.defaults && typeof r.defaults.video_codec === 'number' ? r.defaults.video_codec : 0;
  selCodec.value = availCodecs.has(wantCodec) || wantCodec === 0 ? String(wantCodec) : '0';

  renderVideoOptions();

  const selAudio = $('selAudio');
  selAudio.innerHTML = '';
  (r.audios || []).forEach((s) => {
    const o = document.createElement('option');
    o.value = s.id;
    o.textContent = optionLabel(s);
    selAudio.appendChild(o);
  });
  if (!selAudio.options.length) {
    const o = document.createElement('option');
    o.value = '0';
    o.textContent = '默认音轨';
    selAudio.appendChild(o);
  }
  const wantAudio = r.defaults && r.defaults.audio_id;
  if (wantAudio) {
    for (const o of selAudio.options) {
      if (Number(o.value) === wantAudio) {
        selAudio.value = o.value;
        break;
      }
    }
  }
}

async function addDownload() {
  if (!state.parsed) return;
  const btn = $('btnDownload');
  btn.disabled = true;
  const r = state.parsed;
  const isPgc = r.kind === 'pgc';
  const input = r.resolved_input || $('input').value.trim();
  const qn = Number($('selQn').value || 0);
  const videoCodec = Number($('selCodec').value || 0);
  const audioId = Number($('selAudio').value || 0);
  const ids = Array.from(state.selectedIds);

  try {
    const payload = { input, qn, video_codec: videoCodec, audio_id: audioId };
    if (isPgc) {
      payload.ep_ids = ids.length ? ids : [state.activeEpId || r.default_ep || 0];
    } else if (r.video && r.video.pages && r.video.pages.length > 1) {
      payload.pages = ids.length ? ids : [r.page || 1];
    } else {
      payload.page = r.page || 1;
    }
    const resp = await api('api/tasks', {
      method: 'POST',
      body: JSON.stringify(payload),
    });
    const cnt = resp.count || 1;
    toast(cnt > 1 ? '已批量加入 ' + cnt + ' 个下载任务' : '已加入下载队列');
  } catch (e) {
    toast('加入失败：' + e.message, true);
  } finally {
    updateDownloadBtn();
  }
}

/* ---------- 任务队列 ---------- */
const CHIP = {
  queued: ['排队中', 'chip'],
  downloading: ['下载中', 'chip chip-running'],
  muxing: ['混流中', 'chip chip-running'],
  done: ['已完成', 'chip chip-done'],
  failed: ['失败', 'chip chip-err'],
  paused: ['已暂停', 'chip chip-warn'],
};

function pct(t) {
  if (!t.total) return t.status === 'done' ? 100 : 0;
  return Math.min(100, Math.round((t.downloaded / t.total) * 100));
}

function isActiveStatus(s) {
  return s === 'downloading' || s === 'muxing' || s === 'queued' || s === 'paused' || s === 'failed';
}

function renderTasks(tasks) {
  state.tasks = tasks || [];

  const cntAll = state.tasks.length;
  const cntActive = state.tasks.filter((t) => t.status === 'downloading' || t.status === 'muxing' || t.status === 'queued').length;
  const cntDone = state.tasks.filter((t) => t.status === 'done').length;

  $('cntAll').textContent = String(cntAll);
  $('cntActive').textContent = String(cntActive);
  $('cntDone').textContent = String(cntDone);
  $('btnClearDone').classList.toggle('hidden', cntDone === 0);

  const totalSpd = state.tasks.reduce((acc, t) => acc + (t.status === 'downloading' ? t.speed || 0 : 0), 0);
  const spdEl = $('totalSpeed');
  if (totalSpd > 0) {
    spdEl.textContent = '↓ ' + fmtBytes(totalSpd) + '/s';
    spdEl.classList.remove('hidden');
  } else {
    spdEl.classList.add('hidden');
  }

  let visible = state.tasks;
  if (state.filter === 'active') visible = state.tasks.filter((t) => isActiveStatus(t.status));
  else if (state.filter === 'done') visible = state.tasks.filter((t) => t.status === 'done');

  const list = $('taskList');
  if (!visible.length) {
    if (!state.tasks.length) {
      list.innerHTML =
        '<div class="empty-state">' +
        '<div class="empty-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg></div>' +
        '<p class="empty-title">暂无下载任务</p>' +
        '<p class="empty-sub">在上方粘贴 B 站视频或番剧链接即可解析画质并无损混流到 NAS。</p>' +
        '<div class="empty-tags"><span>BV 视频 / 多P合集</span><span>ep / ss / md 番剧影视</span><span>b23.tv 分享短链</span><span>4K / 8K / Hi-Res 无损</span></div>' +
        '</div>';
    } else {
      list.innerHTML = '<div class="empty-state"><p class="empty-sub">当前筛选下没有任务</p></div>';
    }
    return;
  }

  list.innerHTML = visible
    .map((t) => {
      const [label, cls] = CHIP[t.status] || ['未知', 'chip'];
      const p = pct(t);
      const isDone = t.status === 'done';
      const isRunning = t.status === 'downloading' || t.status === 'muxing' || t.status === 'queued';
      const sizeText = t.total
        ? isDone
          ? fmtBytes(t.total)
          : fmtBytes(t.downloaded) + ' / ' + fmtBytes(t.total) + ' (' + p + '%)'
        : '';
      const spd = t.status === 'downloading' ? fmtSpeed(t.speed) : '';

      let subline = '';
      if (t.error) {
        subline = '<span class="task-err">' + esc(t.error) + '</span>';
      } else if (t.warning) {
        subline = '<span class="task-warn">' + esc(t.warning) + '</span>';
      } else if (isDone && t.output_path) {
        const fname = t.output_path.split(/[\\/]/).pop();
        subline = '<span>' + esc(fname) + (sizeText ? ' · ' + sizeText : '') + '</span>';
      } else {
        subline =
          '<span>' +
          esc(t.stage || '') +
          (sizeText ? ' · ' + sizeText : '') +
          (spd ? ' · ' + spd : '') +
          '</span>';
      }

      let actions = '';
      if (isRunning) {
        actions += '<button class="btn btn-ghost btn-xs" data-act="pause" data-id="' + t.id + '">暂停</button>';
      }
      if (t.status === 'paused' || t.status === 'failed') {
        actions += '<button class="btn btn-soft btn-xs" data-act="retry" data-id="' + t.id + '">重试</button>';
      }
      if (isDone && t.output_path) {
        actions +=
          '<a class="btn btn-soft btn-xs" href="' +
          apiURL('api/tasks/file?id=' + t.id) +
          '">保存到本地</a>';
        actions +=
          '<button class="btn btn-ghost btn-xs" data-act="remove" data-id="' +
          t.id +
          '" title="仅移除列表记录，保留 NAS 上的文件">移除</button>';
      } else {
        actions +=
          '<button class="btn btn-danger btn-xs" data-act="delete" data-id="' +
          t.id +
          '">删除</button>';
      }

      // 已完成的任务不显示 900px 宽的粉红进度条，改用紧凑清爽的信息条
      const barHtml = isDone
        ? ''
        : '<div class="bar' +
          (t.status === 'muxing' ? ' is-muxing' : '') +
          '"><i style="width:' +
          p +
          '%"></i></div>';

      const idTag = t.is_pgc && t.ep_id ? 'ep' + t.ep_id : t.bvid || '';

      return (
        '<div class="task">' +
        '<div class="task-thumb"><img src="' +
        esc(imgURL(t.cover)) +
        '" alt=""></div>' +
        '<div class="task-body">' +
        '<div class="task-top">' +
        '<span class="task-name" title="' +
        esc(t.title) +
        '">' +
        esc(t.page_title || t.title) +
        '</span>' +
        '<span class="' +
        cls +
        '">' +
        label +
        '</span>' +
        '</div>' +
        '<div class="task-meta">' +
        (t.quality_label ? '<span class="meta-tag">' + esc(t.quality_label) + '</span>' : '') +
        (t.video_codec > 0 && t.codec_label ? '<span class="meta-tag">' + esc(t.codec_label) + '</span>' : '') +
        (t.audio_label ? '<span class="meta-tag">' + esc(t.audio_label) + '</span>' : '') +
        (idTag ? '<span>' + esc(idTag) + '</span>' : '') +
        '</div>' +
        barHtml +
        '<div class="task-subfoot">' +
        subline +
        '<span class="task-actions">' +
        actions +
        '</span>' +
        '</div>' +
        '</div>' +
        '</div>'
      );
    })
    .join('');
}

async function taskAction(id, action) {
  try {
    await api('api/tasks/action', { method: 'POST', body: JSON.stringify({ id, action }) });
    if (action === 'delete') toast('已删除任务');
    if (action === 'remove') toast('已从列表移除（保留本地文件）');
    if (action === 'clear_done') toast('已清理已完成记录');
  } catch (e) {
    toast(e.message, true);
  }
}

/* ---------- 设置 ---------- */
async function loadDirCandidates() {
  const sel = $('setDirPick');
  try {
    const d = await api('api/dirs');
    const cands = d.candidates || [];
    if (!cands.length) {
      sel.innerHTML = '<option value="">未检测到有写权限的共享文件夹</option>';
    } else {
      sel.innerHTML = '<option value="">选择有写权限的共享文件夹…</option>';
      cands.forEach((c) => {
        const o = document.createElement('option');
        o.value = c.path;
        o.textContent = c.path + (c.free_gb ? ' · 余 ' + c.free_gb + ' GB' : '');
        sel.appendChild(o);
      });
    }
    if (d.current) {
      $('setDir').value = d.current;
      for (const o of sel.options) if (o.value === d.current) sel.value = d.current;
    }
  } catch (_) {
    sel.innerHTML = '<option value="">无法读取共享文件夹</option>';
  }
}

async function testDir(path) {
  const box = $('dirResult');
  box.classList.remove('hidden');
  box.className = 'field-result';
  box.textContent = '检测中…';
  try {
    const r = await api('api/dir/test', { method: 'POST', body: JSON.stringify({ path }) });
    box.className = 'field-result ' + (r.ok ? 'ok' : 'err');
    box.textContent = r.ok
      ? '✓ ' + r.message + (r.free_gb ? '，可用空间 ' + r.free_gb + ' GB' : '')
      : '✗ ' + r.message;
  } catch (e) {
    box.className = 'field-result err';
    box.textContent = '✗ ' + e.message;
  }
}

async function openSettings() {
  try {
    const s = await api('api/settings');
    $('dirResult').classList.add('hidden');
    loadDirCandidates();
    state.defaultUA = s.default_user_agent || '';
    $('setDir').value = s.download_dir || '';
    $('setTpl').value = s.filename_template || '';
    $('setQn').value = String(s.qn || 80);
    $('setCodec').value = String(typeof s.video_codec === 'number' ? s.video_codec : 0);
    $('setAudio').value = String(s.audio_id || 30280);
    $('setConc').value = String(s.chunk_concurrency || 4);
    $('setTasks').value = String(s.max_tasks || 2);
    $('setBw').value = String(s.bandwidth_kb || 0);
    $('setUA').value = s.user_agent || state.defaultUA;
    $('setProxy').value = s.proxy || '';
    const names = s.cookie_names || [];
    $('cookieInfo').textContent = names.length
      ? '已保存凭据字段：' + names.join(', ') + '（存储于 NAS 本地 var/config.json，权限 0600）'
      : '当前尚未登录。扫码登录后仅保存在 NAS 本地，退出登录会立即清除。';
    $('settingsModal').classList.remove('hidden');
  } catch (e) {
    toast(e.message, true);
  }
}

async function saveSettings() {
  const dir = $('setDir').value.trim();
  const maxTasks = Math.min(10, Math.max(1, Number($('setTasks').value) || 2));
  const chunkConc = Math.min(32, Math.max(1, Number($('setConc').value) || 4));
  const bw = Math.max(0, Number($('setBw').value) || 0);

  if (dir) {
    try {
      const r = await api('api/dir/test', { method: 'POST', body: JSON.stringify({ path: dir }) });
      if (!r.ok) {
        toast('目录不可写：' + r.message, true);
        return;
      }
    } catch (e) {
      toast(e.message, true);
      return;
    }
  }
  try {
    await api('api/settings', {
      method: 'POST',
      body: JSON.stringify({
        download_dir: dir,
        filename_template: $('setTpl').value.trim(),
        qn: Number($('setQn').value),
        video_codec: Number($('setCodec').value) || 0,
        audio_id: Number($('setAudio').value),
        chunk_concurrency: chunkConc,
        max_tasks: maxTasks,
        bandwidth_kb: bw,
        user_agent: $('setUA').value.trim(),
        proxy: $('setProxy').value.trim(),
      }),
    });
    toast('设置已保存');
    $('settingsModal').classList.add('hidden');
    loadStatus();
  } catch (e) {
    toast(e.message, true);
  }
}

/* ---------- 事件绑定 ---------- */
function bind() {
  $('btnLogin').onclick = openLogin;
  $('btnSettings').onclick = openSettings;
  $('btnDirPill').onclick = openSettings;
  $('btnFixDir').onclick = openSettings;
  $('btnParse').onclick = doParse;
  $('btnCloseResult').onclick = closeResult;
  $('btnDownload').onclick = addDownload;
  $('btnRefreshQr').onclick = refreshQR;
  $('btnSaveSettings').onclick = saveSettings;
  $('btnDirTest').onclick = () => testDir($('setDir').value.trim());
  $('btnClearDone').onclick = () => taskAction('', 'clear_done');
  $('selCodec').onchange = () => renderVideoOptions(Number($('selQn').value || 0));
  $('btnResetUA').onclick = () => {
    if (state.defaultUA) $('setUA').value = state.defaultUA;
    toast('已填入默认 User-Agent');
  };

  $('input').addEventListener('input', () => {
    $('btnClearInput').classList.toggle('hidden', !$('input').value.trim());
  });
  $('btnClearInput').onclick = () => {
    $('input').value = '';
    $('btnClearInput').classList.add('hidden');
    $('input').focus();
  };
  $('input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') doParse();
  });

  $('setDirPick').onchange = (e) => {
    if (e.target.value) {
      $('setDir').value = e.target.value;
      testDir(e.target.value);
    }
  };

  document.querySelectorAll('.token').forEach((b) => {
    b.onclick = () => {
      const el = $('setTpl');
      const t = b.dataset.token;
      const start = el.selectionStart == null ? el.value.length : el.selectionStart;
      const end = el.selectionEnd == null ? el.value.length : el.selectionEnd;
      el.value = el.value.slice(0, start) + t + el.value.slice(end);
      el.focus();
      el.selectionStart = el.selectionEnd = start + t.length;
    };
  });

  $('btnLogout').onclick = async () => {
    await logout();
    $('settingsModal').classList.add('hidden');
  };

  document.querySelectorAll('[data-close]').forEach((b) => {
    b.onclick = () => $(b.dataset.close).classList.add('hidden');
  });
  document.querySelectorAll('.modal').forEach((m) => {
    m.addEventListener('click', (e) => {
      if (e.target === m) m.classList.add('hidden');
    });
  });

  document.querySelectorAll('.queue-tabs .tab').forEach((tab) => {
    tab.onclick = () => {
      state.filter = tab.dataset.filter;
      document.querySelectorAll('.queue-tabs .tab').forEach((x) => x.classList.remove('active'));
      tab.classList.add('active');
      renderTasks(state.tasks);
    };
  });

  $('taskList').addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    taskAction(btn.dataset.id, btn.dataset.act);
  });
}

function subscribe() {
  try {
    const es = new EventSource(apiURL('api/events'));
    es.onmessage = (ev) => {
      try {
        const data = JSON.parse(ev.data);
        renderTasks(data.tasks);
        setCooldown(data.cooldown || 0);
      } catch (_) {
        /* ignore */
      }
    };
    es.onerror = () => {};
  } catch (_) {
    setInterval(() => api('api/tasks').then((r) => renderTasks(r.tasks)).catch(() => {}), 3000);
  }
}

bind();
loadStatus();
subscribe();
setInterval(loadStatus, 20000);
