// A fake Stackchan on Web Serial for the page tests (page_test.go): answers the robot's USB setup
// protocol (hello, status, provision, restart; firmware usb_setup.h) and records every request in
// window.__usbLog. The robot's id and firmware come from window.__fakeRobot (set before this runs),
// with "ungranted", "cancel" and "silent" (no answer as Embody Mode) for the port chooser's cases.
// It cannot be flashed: the installer gets no answer.
(() => {
  const enc = new TextEncoder(), dec = new TextDecoder();
  window.__usbLog = [];
  class FakePort {
    constructor(id, firmware) { this.id = id; this.firmware = firmware; this.readable = null; this.writable = null; }
    getInfo() { return { usbVendorId: 0x303a, usbProductId: 0x1001 }; }
    async setSignals() {}
    async open() {
      let ctl;
      this.readable = new ReadableStream({ start: (c) => { ctl = c; } });
      const port = this;
      let buf = "";
      this.writable = new WritableStream({ write(chunk) {
        buf += dec.decode(chunk);
        let nl;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl); buf = buf.slice(nl + 1);
          const i = line.indexOf("@stackchan ");
          if (i < 0) continue;
          const req = JSON.parse(line.slice(i + 11));
          window.__usbLog.push(req);
          let res = { ok: true };
          if (req.op === "hello" && cfg.silent) continue; // not Embody Mode: no answer
          if (req.op === "hello") res = { ok: true, id: port.id, model: "stackchan-cores3", firmware: port.firmware, protocol: 1 };
          if (req.op === "provision") res = { ok: true, applied: Object.keys(req).filter((k) => k !== "op") };
          if (req.op === "provision" && cfg.refuseOnce && !port.refused) { // nobody tapped Yes in time
            port.refused = true;
            res = { ok: false, error: "not confirmed on the robot: nothing changed" };
          }
          setTimeout(() => ctl.enqueue(enc.encode("log line\r\n@stackchan " + JSON.stringify(res) + "\r\n")), 50);
        }
      } });
    }
    async close() { this.readable = null; this.writable = null; }
  }
  const cfg = window.__fakeRobot || { id: "stackchan-0a1b2c3d4e50", firmware: "v0.2.0" };
  const port = new FakePort(cfg.id, cfg.firmware);
  let granted = !cfg.ungranted;
  Object.defineProperty(navigator, "serial", { value: {
    // cfg.ungranted: Chrome has not been allowed this port yet (a new computer): getPorts is empty
    // until the chooser picks it; cfg.cancel: the chooser is closed without a choice.
    getPorts: async () => (granted ? [port] : []),
    requestPort: async () => {
      if (cfg.cancel) throw new DOMException("No port selected by the user.", "NotFoundError");
      granted = true;
      return port;
    },
    addEventListener() {},
  } });
})();
