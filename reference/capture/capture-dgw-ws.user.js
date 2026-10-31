// ==UserScript==
// @name         Capture meta.ai DGW websocket frames
// @namespace    local
// @version      1.0
// @match        https://meta.ai/*
// @match        https://www.meta.ai/*
// @run-at       document-start
// @grant        none
// ==/UserScript==

(function () {
  'use strict';

  // Every frame the page sends or receives, in arrival order.
  //
  // Persisted to sessionStorage because the page can reload mid-capture (the
  // composer navigates to /prompt/<id> when a message is sent, which was losing
  // everything recorded before the reply arrived).
  const KEY = '__dgw_frames_v1';
  let frames = [];
  try {
    const saved = sessionStorage.getItem(KEY);
    if (saved) frames = JSON.parse(saved);
  } catch (e) { frames = []; }

  let saveTimer = null;
  function persist() {
    if (saveTimer) return;
    saveTimer = setTimeout(() => {
      saveTimer = null;
      try { sessionStorage.setItem(KEY, JSON.stringify(frames)); } catch (e) {}
    }, 100);
  }

  // Write immediately, ignoring the debounce, so a dump taken right after a
  // frame is not missing it.
  function flush() {
    if (saveTimer) { clearTimeout(saveTimer); saveTimer = null; }
    try { sessionStorage.setItem(KEY, JSON.stringify(frames)); } catch (e) {}
  }

  // Re-read from storage so a helper called after a reload sees everything,
  // even if this closure's array is stale (the script can be evaluated twice
  // on one page: once at document-start, once after a soft navigation).
  function sync() {
    flush();
    try {
      const saved = sessionStorage.getItem(KEY);
      if (saved) frames = JSON.parse(saved);
    } catch (e) {}
    return frames;
  }

  // A DGW frame starts with its type byte. Anything from StreamGroup_EstabStream
  // upward is a grouped stream frame; the rest are control frames we keep too,
  // because they are cheap and make the log readable.
  const NAME = {
    2: 'Empty', 3: 'Drain', 4: 'Deauth', 5: 'DeprecatedEstabStream',
    6: 'DeprecatedData', 7: 'SmallAck', 8: 'DeprecatedEndOfData',
    9: 'Ping', 10: 'Pong', 12: 'StreamGroup_Ack',
    13: 'StreamGroup_Data', 14: 'StreamGroup_EndOfData', 15: 'StreamGroup_EstabStream',
  };

  function toB64(data) {
    // data may be ArrayBuffer, Blob or a typed array depending on binaryType.
    if (data instanceof ArrayBuffer) {
      return btoa(String.fromCharCode.apply(null, new Uint8Array(data)));
    }
    if (ArrayBuffer.isView(data)) {
      return btoa(String.fromCharCode.apply(null, new Uint8Array(
        data.buffer, data.byteOffset, data.byteLength)));
    }
    return null; // Blob: handled by the async path below
  }

  function record(dir, data) {
    const b64 = toB64(data);
    if (b64) {
      push(dir, b64);
      return;
    }
    if (data && typeof data.arrayBuffer === 'function') {
      data.arrayBuffer().then(buf => push(dir, toB64(buf))).catch(() => {});
      return;
    }
    // A text frame is not something this decoder handles, but log it so the
    // capture is not silently incomplete.
    push(dir, 'TEXT:' + String(data).slice(0, 500));
  }

  // The page may deliver the same message through both addEventListener and
  // onmessage (a library wrapping them, or a re-dispatch). Identical bytes in
  // the same direction within a few milliseconds are one delivery.
  let lastDir = null;
  let lastB64 = null;
  let lastAt = 0;

  function push(dir, b64) {
    const now = Date.now();
    if (dir === lastDir && b64 === lastB64 && now - lastAt < 20) {
      lastAt = now;
      return; // duplicate delivery of one message
    }
    lastDir = dir;
    lastB64 = b64;
    lastAt = now;

    let type = -1;
    let len = 0;
    if (!b64.startsWith('TEXT:')) {
      try {
        const raw = atob(b64);
        type = raw.charCodeAt(0);
        // len is the 3-byte little-endian field after type + streamId.
        len = raw.charCodeAt(3) | (raw.charCodeAt(4) << 8) | (raw.charCodeAt(5) << 16);
      } catch (e) { /* leave type/len at defaults */ }
    }
    frames.push({
      direction: dir,
      type: type,
      typeName: NAME[type] || (type >= 0 ? 'type' + type : 'text'),
      len: len,
      bytes: b64.startsWith('TEXT:') ? 0 : Math.floor(b64.length * 3 / 4),
      b64: b64,
      at: new Date().toISOString(),
    });
    persist();
    console.log('[dgw] ' + dir + ' ' + (NAME[type] || type) + ' len=' + len);
  }

  // ---- outgoing: WebSocket.prototype.send ----
  const origSend = WebSocket.prototype.send;
  WebSocket.prototype.send = function (data) {
    try { record('out', data); } catch (e) { console.warn('[dgw] send hook', e); }
    return origSend.apply(this, arguments);
  };

  // ---- incoming: both paths, because libraries use either one ----
  const origAdd = WebSocket.prototype.addEventListener;
  WebSocket.prototype.addEventListener = function (type, listener, opts) {
    if (type === 'message' && typeof listener === 'function') {
      const wrapped = function (ev) {
        try { record('in', ev.data); } catch (e) { console.warn('[dgw] listener hook', e); }
        return listener.apply(this, arguments);
      };
      return origAdd.call(this, type, wrapped, opts);
    }
    return origAdd.apply(this, arguments);
  };

  // Some code sets onmessage directly; wrap the setter too.
  const desc = Object.getOwnPropertyDescriptor(WebSocket.prototype, 'onmessage');
  if (desc && desc.set) {
    Object.defineProperty(WebSocket.prototype, 'onmessage', {
      configurable: true,
      enumerable: desc.enumerable,
      get: desc.get,
      set(fn) {
        if (typeof fn !== 'function') return desc.set.call(this, fn);
        return desc.set.call(this, function (ev) {
          try { record('in', ev.data); } catch (e) { console.warn('[dgw] onmessage hook', e); }
          return fn.apply(this, arguments);
        });
      },
    });
  }

  // ---- download: one click, everything at once ----
  window.__dgwDump = function () {
    sync();
    const lines = frames.map(f => [f.direction, f.type, f.typeName, f.len, f.bytes, f.at, f.b64].join('\t'));
    const header = ['direction', 'type', 'typeName', 'declared_len', 'byte_len', 'at', 'b64'].join('\t');
    const text = header + '\n' + lines.join('\n');

    const blob = new Blob([text], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'dgw-frames-' + frames.length + '.txt';
    (document.body || document.documentElement).appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60000);

    // Summary so you can see immediately whether the reply text was captured.
    const inbound = frames.filter(f => f.direction === 'in').length;
    console.log('[dgw] dumped ' + frames.length + ' frames (' + inbound + ' inbound)');
    return frames.length;
  };

  // Convenience: only the inbound blocks, plus a reminder if there are none.
  window.__dgwInbound = function () {
    const inb = sync().filter(f => f.direction === 'in');
    if (!inb.length) {
      console.warn('[dgw] no inbound frames yet — send a message and wait for the reply to finish');
      return 0;
    }
    console.table(inb.map(f => ({ type: f.typeName, len: f.len, bytes: f.bytes })));
    return inb.length;
  };

  window.__dgwReset = function () {
    frames = [];
    try { sessionStorage.removeItem(KEY); } catch (e) {}
    console.log('[dgw] cleared');
  };

  console.log('[dgw] websocket capture armed (' + frames.length + ' frames restored). ' +
    'Send a message, WAIT for the whole reply, then run __dgwDump().');

  // Warn about the two ways a capture ends up incomplete.
  window.__dgwCheck = function () {
    sync();
    const outData = frames.filter(f => f.direction === 'out' && f.type === 13).length;
    const inData = frames.filter(f => f.direction === 'in' && f.type === 13).length;
    const inEnd = frames.filter(f => f.direction === 'in' && f.type === 14).length;
    console.log('[dgw] out Data=' + outData + '  in Data=' + inData + '  in EndOfData=' + inEnd);
    if (outData === 0) console.warn('[dgw] no outbound prompt frame — you dumped before sending?');
    if (inData === 0) console.warn('[dgw] no inbound Data — the reply has not arrived yet; wait longer');
    if (inEnd === 0) console.warn('[dgw] no EndOfData — the reply may still be streaming');
    return { outData, inData, inEnd };
  };
})();
