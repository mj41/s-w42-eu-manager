// usb.js: setting a Stackchan up over its USB cable from the browser (Web Serial): the robot's
// setup protocol (firmware usb_setup.h), the official firmware, flashing (esptool-js), backups.
// No page code here: progress and log go through hooks {step(i), progress(0..1 | null), log(text),
// backup(blob, name, first)}.
import { ESPLoader, Transport } from "/vendor/esptool-js-0.7.0-repro-mj41cz.js";

const PREFIX = "@stackchan ";
export const ESPRESSIF = 0x303a; // the USB vendor id of the robot's ESP32-S3
export const supported = () => "serial" in navigator;
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
export const quiet = { step() {}, progress() {}, log() {}, backup() {} };

// The robots this page may use already (granted before; no chooser), and the chooser.
export async function knownPorts() {
  return supported() ? (await navigator.serial.getPorts()).filter((p) => p.getInfo().usbVendorId === ESPRESSIF) : [];
}
export const choosePort = () => navigator.serial.requestPort({ filters: [{ usbVendorId: ESPRESSIF }] });

// --- the robot's USB setup protocol (firmware usb_setup.h) ---
export class Robot {
  constructor(port) { this.port = port; this.waiting = null; this.buf = ""; }
  async open({ reset = false } = {}) {
    if (!this.port.readable) await this.port.open({ baudRate: 115200 });
    if (reset) { // a hard reset as esptool does it on the USB-JTAG port: RTS pulse with DTR low
      await this.port.setSignals({ dataTerminalReady: false, requestToSend: true });
      await new Promise((r) => setTimeout(r, 100));
      await this.port.setSignals({ dataTerminalReady: false, requestToSend: false });
    }
    this.reader = this.port.readable.getReader();
    this.writer = this.port.writable.getWriter();
    this.reading = (async () => {
      const td = new TextDecoder();
      try {
        for (;;) {
          const { value, done } = await this.reader.read();
          if (done) break;
          this.buf += td.decode(value, { stream: true });
          let nl;
          while ((nl = this.buf.search(/[\r\n]/)) >= 0) {
            const line = this.buf.slice(0, nl);
            this.buf = this.buf.slice(nl + 1);
            const i = line.indexOf(PREFIX);
            if (i >= 0 && this.waiting) {
              try { this.waiting(JSON.parse(line.slice(i + PREFIX.length))); } catch {}
            }
          }
        }
      } catch {}
    })();
  }
  // ask sends a request and waits for the answer, repeating it while the robot boots.
  async ask(req, timeoutMs = 30000, repeat = true) {
    const line = new TextEncoder().encode(PREFIX + JSON.stringify(req) + "\n");
    return new Promise((resolve, reject) => {
      let timer, again;
      const finish = (fn, v) => { clearTimeout(timer); clearInterval(again); this.waiting = null; fn(v); };
      this.waiting = (res) => finish(resolve, res);
      timer = setTimeout(() => finish(reject, new Error("The robot did not answer. Is it on? Does it have Embody Mode with setup over USB?")), timeoutMs);
      const send = () => this.writer.write(line).catch(() => {});
      send();
      if (repeat) again = setInterval(send, 2000);
    });
  }
  async close() {
    try { await this.reader.cancel(); } catch {}
    try { this.reader.releaseLock(); this.writer.releaseLock(); } catch {}
    await this.reading;
    try { await this.port.close(); } catch {}
  }
}


// posixTZ is this browser's time zone as a POSIX TZ string for the robot (e.g.
// "CET-1CEST,M3.5.0,M10.5.0/3"): the offsets from January and July, with the EU or US summer
// time rules for zones that have it (others: the winter offset only).
export function posixTZ(now = new Date()) {
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  const year = now.getFullYear();
  const jan = -new Date(year, 0, 1).getTimezoneOffset(), jul = -new Date(year, 6, 1).getTimezoneOffset(); // minutes east
  const off = (east) => { // POSIX: hours WEST of UTC, the sign flipped
    const m = -east, sign = m < 0 ? "-" : "", a = Math.abs(m);
    return sign + Math.floor(a / 60) + (a % 60 ? ":" + String(a % 60).padStart(2, "0") : "");
  };
  const std = Math.min(jan, jul), dst = Math.max(jan, jul);
  if (std === dst) return `UTC${off(std)}`;
  if (jan > jul) return `UTC${off(std)}`; // southern summer time: winter offset only (rules vary)
  // The EU changes at 01:00 UTC: local hour = offset + 1 (winter time in March, summer time in October).
  if (zone.startsWith("Europe/")) return `STD${off(std)}DST${off(dst)},M3.5.0/${std / 60 + 1},M10.5.0/${dst / 60 + 1}`;
  if (zone.startsWith("America/")) return `STD${off(std)}DST${off(dst)},M3.2.0,M11.1.0`;
  return `UTC${off(std)}`;
}

// hello: who the robot on this port is (id, firmware, its manager settings), or null when it
// does not answer (another firmware, or busy). Read-only; no reset.
export async function hello(port, timeoutMs = 6000) {
  const r = new Robot(port);
  try {
    await r.open();
    return await r.ask({ op: "hello" }, timeoutMs);
  } catch { return null; } finally { try { await r.close(); } catch {} }
}

// --- firmware ---
const hex = (buf) => Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, "0")).join("");
export const sha256 = async (data) => hex(await crypto.subtle.digest("SHA-256", data));

export async function firmwareFiles() {
  const m = await fetch("/firmware/manifest.json").then((r) => {
    if (r.status === 401) throw new Error("Open this page on the computer the manager runs on to install the firmware.");
    if (!r.ok) throw new Error("This manager has no firmware to install: untick \"Install the latest official Embody Mode\".");
    return r.json();
  });
  const files = [];
  for (const p of m.parts) {
    const data = new Uint8Array(await fetch("/firmware/" + p.path).then((r) => { if (!r.ok) throw new Error("download " + p.path); return r.arrayBuffer(); }));
    if (await sha256(data) !== p.sha256) throw new Error(`${p.path}: the download is damaged (checksum), try again`);
    files.push({ data, address: p.offset });
  }
  return files;
}

// The installer (esptool-js) on the robot's USB port, for one session: backup, then write.
// A second try on a fresh connection if the first cannot reach the robot's flasher.
async function installer(port, hooks) {
  for (let attempt = 1; ; attempt++) {
    const transport = new Transport(port, false);
    const loader = new ESPLoader({ transport, baudrate: 921600, serialOptions: { bufferSize: 64 * 1024 },
      terminal: { clean() {}, writeLine: (l) => hooks.log(l + "\n"), write: (t) => hooks.log(t) } });
    let timer;
    try {
      hooks.log(`installer: connecting (try ${attempt})\n`);
      const chip = await Promise.race([loader.main(), new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(
        "The robot does not answer the installer.")), 30000); })]).finally(() => clearTimeout(timer));
      if (!/ESP32-S3/i.test(chip)) { await transport.disconnect(); throw Object.assign(new Error(`This is ${chip}, not a Stackchan (ESP32-S3).`), { final: true }); }
      return { loader, transport };
    } catch (e) {
      hooks.log(`installer: ${e.message || e}\n`);
      try { await transport.disconnect(); } catch {}
      if (e.final) throw e;
      if (attempt >= 2) throw new Error("The robot does not answer the installer. Hold its reset button until the green LED lights up, then press the button here again.");
      await sleep(1500);
    }
  }
}

async function writeFlash(loader, files, hooks) {
  hooks.progress(0);
  const total = files.reduce((n, f) => n + f.data.length, 0);
  await loader.writeFlash({
    fileArray: files, flashMode: "keep", flashFreq: "keep", flashSize: "keep", eraseAll: false, compress: true,
    calculateMD5Hash: md5hex, // every part read back from the flash as MD5 and compared
    reportProgress: (i, written) => {
      hooks.progress((files.slice(0, i).reduce((n, f) => n + f.data.length, 0) + written) / total);
    },
  });
  hooks.progress(null);
  await loader.after("hard_reset"); // our esptool-js pulses RTS (0.7.0 left the chip in the flasher)
}

// --- backup of the robot's current firmware ---
// The whole 16 MB flash as one image for address 0x0 (esptool can write it too), read
// sparsely: the robot hashes each 64 KB block, and erased blocks (all 0xFF) are not read.
export const FLASH_SIZE = 16 * 1024 * 1024, BLOCK = 64 * 1024;
const ERASED_MD5 = "ecb99e6ffea7be1e5419350f725da86b"; // MD5 of 64 KB of 0xFF

// MD5 (RFC 1321), only to check backup blocks against the flasher's digest (not for security).
function md5hex(data) {
  const K = new Uint32Array(64), S = [7, 12, 17, 22, 5, 9, 14, 20, 4, 11, 16, 23, 6, 10, 15, 21];
  for (let i = 0; i < 64; i++) K[i] = Math.floor(Math.abs(Math.sin(i + 1)) * 2 ** 32) >>> 0;
  const n = data.length, padded = new Uint8Array(((n + 8) >> 6) * 64 + 64);
  padded.set(data);
  padded[n] = 0x80;
  const dv = new DataView(padded.buffer);
  dv.setUint32(padded.length - 8, (n * 8) >>> 0, true);
  dv.setUint32(padded.length - 4, Math.floor(n / 0x20000000), true);
  let a0 = 0x67452301, b0 = 0xefcdab89, c0 = 0x98badcfe, d0 = 0x10325476;
  const M = new Uint32Array(16);
  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) M[i] = dv.getUint32(off + i * 4, true);
    let a = a0, b = b0, c = c0, d = d0;
    for (let i = 0; i < 64; i++) {
      let f, g;
      if (i < 16) { f = (b & c) | (~b & d); g = i; }
      else if (i < 32) { f = (d & b) | (~d & c); g = (5 * i + 1) % 16; }
      else if (i < 48) { f = b ^ c ^ d; g = (3 * i + 5) % 16; }
      else { f = c ^ (b | ~d); g = (7 * i) % 16; }
      const s = S[(i >> 4) * 4 + (i % 4)];
      const t = (a + f + K[i] + M[g]) >>> 0;
      a = d; d = c; c = b;
      b = (b + ((t << s) | (t >>> (32 - s)))) >>> 0;
    }
    a0 = (a0 + a) >>> 0; b0 = (b0 + b) >>> 0; c0 = (c0 + c) >>> 0; d0 = (d0 + d) >>> 0;
  }
  const out = new DataView(new ArrayBuffer(16));
  [a0, b0, c0, d0].forEach((v, i) => out.setUint32(i * 4, v, true));
  return Array.from(new Uint8Array(out.buffer), (x) => x.toString(16).padStart(2, "0")).join("");
}

// session: {loader, transport}, replaced by a new installer session when a step fails (up to
// 3 times), which goes on where it stopped.
async function readBackup(session, reconnect, hooks) {
  const image = new Uint8Array(FLASH_SIZE).fill(0xff);
  const md5 = new Map(); // block address -> MD5, for the blocks in use
  let retries = 0;
  const retry = async (what, e) => {
    hooks.log(`backup: ${what} failed: ${e.message || e}\n`);
    if (++retries > 3) throw new Error("The backup failed: the robot stopped answering. Keep this page in front while it runs, and try again.");
    session = await reconnect(session);
  };
  hooks.progress(0);
  const t0 = Date.now();
  for (let a = 0; a < FLASH_SIZE; ) {
    try {
      const sum = (await session.loader.flashMd5sum(a, BLOCK)).toLowerCase();
      if (sum !== ERASED_MD5) md5.set(a, sum);
      a += BLOCK;
      hooks.progress(0.1 * a / FLASH_SIZE);
    } catch (e) { await retry(`checking 0x${a.toString(16)}`, e); }
  }
  hooks.log(`backup: ${md5.size} of ${FLASH_SIZE / BLOCK} blocks in use (${Math.round((Date.now() - t0) / 1000)} s)\n`);
  const used = [...md5.keys()];
  for (let i = 0; i < used.length; ) {
    try {
      // The flasher's MD5 of the block checks what arrived; the scan's MD5 that it did not change.
      const block = await session.loader.readFlash(used[i], BLOCK, null, md5hex);
      if (md5hex(block) !== md5.get(used[i])) throw new Error(`block 0x${used[i].toString(16)} changed since the scan`);
      image.set(block, used[i]);
      i++;
      hooks.progress(0.1 + 0.9 * i / used.length);
      if (i % 16 === 0) hooks.log(`backup: ${i}/${used.length} blocks read (${Math.round((Date.now() - t0) / 1000)} s)\n`);
    } catch (e) { await retry(`reading 0x${used[i].toString(16)}`, e); }
  }
  hooks.progress(null);
  hooks.log(`backup: done in ${Math.round((Date.now() - t0) / 1000)} s\n`);
  return { image, session };
}

// What the image holds: the running app's description (esp_app_desc_t), found through the
// partition table (0x8000) and the OTA data.
export function identify(image) {
  const dv = new DataView(image.buffer), td = new TextDecoder();
  const str = (o, n) => td.decode(image.subarray(o, o + n)).replace(/\0.*$/s, "");
  const apps = [];
  let otadata = -1;
  for (let o = 0x8000; o < 0x9000 && dv.getUint16(o, true) === 0x50aa; o += 32) {
    const type = image[o + 2], sub = image[o + 3], offset = dv.getUint32(o + 4, true);
    if (type === 0) apps.push({ sub, offset });
    if (type === 1 && sub === 0) otadata = offset;
  }
  let app = apps.find((a) => a.sub === 0x10) || apps[0]; // ota_0, or the factory app
  if (otadata >= 0) { // the newest valid OTA entry picks the slot
    const seqs = [0, 0x1000].map((d) => dv.getUint32(otadata + d, true)).filter((s) => s && s !== 0xffffffff);
    if (seqs.length) app = apps.find((a) => a.sub === 0x10 + ((Math.max(...seqs) - 1) % 2)) || app;
  }
  if (!app || dv.getUint32(app.offset + 32, true) !== 0xabcd5432) return {};
  const d = app.offset + 32;
  return { project: str(d + 48, 32), version: str(d + 16, 32), built: `${str(d + 96, 16)} ${str(d + 80, 16)}`,
    idf: str(d + 112, 32), elf_sha256: hex(image.subarray(d + 144, d + 176)) };
}

// After the restart: the pairing link the robot shows, once it is connected to its app (up
// to a minute). The robot comes back as a new USB device, so the port is found again.
export async function pairLink(hooks = quiet) {
  let r = null;
  for (const until = Date.now() + 60000; Date.now() < until; ) {
    await sleep(2000);
    if (!r) {
      const ports = (await navigator.serial.getPorts()).filter((p) => p.getInfo().usbVendorId === 0x303a);
      if (ports.length !== 1) continue;
      r = new Robot(ports[0]);
      try { await r.open(); } catch { r = null; continue; }
    }
    const res = await r.ask({ op: "pair" }, 3000, false).catch(() => null);
    if (res?.ok && res.url) { await r.close(); hooks.log("pairing link received\n"); return res.url; }
    if (!res) { try { await r.close(); } catch {} r = null; } // no answer: the port changed
  }
  if (r) try { await r.close(); } catch {}
  hooks.log("no pairing link within a minute\n");
  return "";
}


// setUp runs a setup on the robot on port. plan:
//   firmware: "latest" (install the official release) or "keep"
//   backup:   save the current firmware first (with "latest"); hooks.backup gets the file
//   apps:     async (hello) => ({servers: [{name, url, token, e2e?}], start: <url>, manager?: {...}}):
//             the apps to write (the manager's answer), given who the robot is
//   autostart, wifi ({ssid, password}): optional; left as they are when undefined
// hooks.step(key) with key "connect", "backup", "firmware", "identify", "apps", "write", "restart".
// Returns {hello, servers, start, wifi}. A new start app asks on the robot's screen (a minute).
export async function setUp(port, plan, hooks = quiet) {
  let robot = null, session = null;
  try {
    hooks.step("connect");
    // Ask the firmware first: Embody Mode answers with the record of the robot's original
    // firmware if a setup made a backup before; any other firmware stays silent.
    const before = plan.backup ? await hello(port, 8000) : null;
    if (plan.backup) await sleep(500); // let the port settle before the installer opens it
    let original = null, previous = null;
    if (plan.firmware === "latest") {
      const files = await firmwareFiles();
      session = await installer(port, hooks);
      if (plan.backup) { // every time it is ticked; the first one is the robot's original
        hooks.step("backup");
        const first = !before?.original;
        const reconnect = async (old) => {
          try { await old.transport.disconnect(); } catch {}
          await sleep(1000);
          return (session = await installer(port, hooks));
        };
        const { image } = await readBackup(session, reconnect, hooks);
        const mac = (await session.loader.chip.readMac(session.loader)).replace(/:/g, "").toLowerCase();
        const now = new Date(), day = now.toISOString().slice(0, 10);
        const stamp = `${day}-${String(now.getHours()).padStart(2, "0")}${String(now.getMinutes()).padStart(2, "0")}`;
        const record = { ...identify(image), sha256: await sha256(image), size: image.length, saved: day };
        if (first) original = record; else previous = record;
        hooks.backup(new Blob([image]), `stackchan-${mac}-${first ? "original" : "backup"}-${stamp}.bin`, first);
      }
      hooks.step("firmware");
      await writeFlash(session.loader, files, hooks);
      await session.transport.disconnect();
      session = null;
    }
    hooks.step("identify");
    robot = new Robot(port);
    await robot.open({ reset: true }); // starts new firmware, or leaves the flasher after an earlier try
    const h = await robot.ask({ op: "hello" }, 40000);
    hooks.step("apps");
    const setup = await plan.apps(h);
    hooks.step("write");
    const all = setup.servers;
    const start = all.find((a) => a.url === setup.start) || all[0];
    let req;
    if (setup.manager) { // the manager's apps, all of them (they replace those it set before), and its key
      // e2e: the robot turns end-to-end encryption on for it (a catalog app marked so).
      req = { op: "provision", servers: all.map(({ name, url, token, e2e }) => ({ name, url, token: token || "", ...(e2e ? { e2e: true } : {}) })),
        pin: start.url, manager: setup.manager };
      if (setup.manager2) req.manager2 = setup.manager2; // may become its manager on its screen
    } else {
      req = { op: "provision", server: { name: start.name, url: start.url, token: start.token || "" }, default: true };
      const others = all.filter((a) => a !== start && a.url);
      if (others.length) req.servers = others.map(({ name, url, token }) => ({ name, url, token: token || "" }));
    }
    if (plan.autostart !== undefined) req.autostart = plan.autostart;
    req.tz = posixTZ(); // the robot's clock and its night hours, as this browser's
    if (plan.wifi?.ssid) req.wifi = plan.wifi;
    if (original) req.original = original;
    if (previous) req.previous = previous;
    await provision(robot, req, hooks, () => writeAgain(port, req, hooks, h, all, start.url));
    hooks.step("restart");
    await robot.ask({ op: "restart" }, 8000, false).catch(() => {});
    return { hello: h, servers: all, start: start.url, wifi: req.wifi };
  } catch (e) {
    hooks.log(`error: ${e.message || e}\n`);
    // Never leave the robot in the flasher: restart it into its firmware.
    try {
      if (session) { await session.transport.disconnect(); session = null; }
      if (!robot) { robot = new Robot(port); await robot.open({ reset: true }); }
    } catch {}
    throw /Failed to open serial port/.test(e.message || "")
      ? new Error("The robot's USB port is busy: another tab or program uses it (another manager tab, idf.py monitor, s-w42-eu-usb). Close it and try again.")
      : e;
  } finally {
    if (session) try { await session.transport.disconnect(); } catch {}
    if (robot) await robot.close(); else try { await port.close(); } catch {}
  }
}

// provision writes the settings. Sent once (the robot answered hello already): a new start app
// waits for a tap on the robot (hooks.asking), and repeated copies would pile up in its USB buffer.
// Not confirmed there: the error has retry(), which asks the robot again with the same settings.
async function provision(robot, req, hooks, again) {
  hooks.log("provision: a new start app asks on the robot's screen; tap Yes there\n");
  hooks.asking?.(60);
  const p = await robot.ask(req, 75000, false).finally(() => hooks.asking?.(0));
  if (p.ok) return;
  if (/not confirmed/.test(p.error || "")) throw Object.assign(new Error(
    "Not confirmed on the robot: nobody tapped Yes on its screen within a minute. Nothing was changed."), { retry: again });
  throw new Error("The robot refused the settings: " + (p.error || "unknown"));
}

// writeAgain repeats only the last steps of a setup (the settings, the restart): firmware, backup
// and the apps' tokens are done already.
async function writeAgain(port, req, hooks, h, all, start) {
  const robot = new Robot(port);
  try {
    hooks.step("write");
    await robot.open();
    await robot.ask({ op: "hello" }, 8000);
    await provision(robot, req, hooks, () => writeAgain(port, req, hooks, h, all, start));
    hooks.step("restart");
    await robot.ask({ op: "restart" }, 8000, false).catch(() => {});
    return { hello: h, servers: all, start, wifi: req.wifi };
  } finally { await robot.close(); }
}

// restore puts back a backup a setup saved (the original or the previous firmware): only a file
// the robot recorded (its checksum), so a wrong file never goes in. Returns the record.
export async function restore(port, file, hooks = quiet) {
  let session = null, robot = null;
  try {
    hooks.step("connect");
    const before = await hello(port, 40000);
    hooks.step("check");
    const records = [before?.original, before?.previous].filter((r) => r?.sha256);
    if (!records.length) throw new Error("This robot has no record of its earlier firmware, so this page cannot check the file. Restore it from a terminal: esptool.py write_flash 0x0 <backup file>.");
    const data = new Uint8Array(await file.arrayBuffer());
    const sum = await sha256(data);
    const match = data.length === FLASH_SIZE && records.find((r) => r.sha256 === sum);
    if (!match) throw new Error("This file is not a backup this robot recorded (original or previous firmware): its checksum differs.");
    hooks.step("firmware");
    session = await installer(port, hooks);
    await writeFlash(session.loader, [{ data, address: 0 }], hooks);
    await session.transport.disconnect();
    session = null;
    hooks.step("restart");
    robot = new Robot(port);
    await robot.open({ reset: true });
    return { ...match, which: match === before.original ? "original" : "previous" };
  } finally {
    if (session) try { await session.transport.disconnect(); } catch {}
    if (robot) await robot.close(); else try { await port.close(); } catch {}
  }
}
