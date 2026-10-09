# Vendored

- `esptool-js-0.7.0-repro-mj41cz.js`: [esptool-js](https://github.com/espressif/esptool-js) 0.7.0 by
  Espressif, Apache-2.0 (`esptool-js-LICENSE`), with two fixes (branch `repro-mj41cz`, commit
  `cc828dc`, to be proposed upstream): `hard_reset` pulls EN low first, and `readFlash` reads (and
  checks) the flasher's MD5 digest after the data. `bundle.js` as `npm ci && npm run build` makes it
  from that commit; SHA-256 `2578b31c07ff130112d161668c4321bbf7b3b14e7e1616a579c8cc297dbccd4a`.
  Used by the setup page (`setup.html`) to flash a robot from the browser; served from this
  server, not a CDN.
