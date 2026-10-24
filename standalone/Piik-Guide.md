# Piik: single-file guide

`Piik.exe` is the whole App in one file: the web interface, the native screen
capture and encode helper, and the public-link tunnel. There is nothing to
install.

## Requirements

- Windows 10 or 11, 64-bit.
- An NVIDIA GPU for hardware encoding. With one present, Piik uses only that GPU
  for H.264, H.265 and AV1. Without one, it uses whatever hardware encoder the
  machine has.
- A browser (Chrome or Edge recommended) to control the share and to watch.

## Start

1. Put `Piik.exe` in a folder you can write to (see "Files").
2. Double-click it. A console window opens and your browser shows the launcher.
3. Choose a mode, then **Start sharing**, pick a screen or window, and send the
   invitation link or room code to your viewers.

Close the console window (or press Ctrl+C in it) to stop Piik.

## Modes

| Mode | What it does |
| --- | --- |
| Local room | Viewers on your network join at the address Piik shows (port 8787). |
| Public invite | Creates one public viewer link through the built-in tunnel. |
| Connect to Site | Uses a hosted Piik Site you give it. |

## Command-line options

Run from a terminal, for example `Piik.exe --link`.

| Option | Meaning |
| --- | --- |
| `--local` | Start the Local room without the mode selector. |
| `--link` | Start a public viewer link without the selector. |
| `--site <url>` | Save and open a Piik Site address. |
| `--window` | Open Piik in its own window instead of the browser (H.264 only). |
| `--debug` | Save diagnostics to rotated log files. |
| `--log-dir <folder>` | Where diagnostics are written. |
| `--port <n>` | Local room port (default 8787). |
| `--lan-address <ip>` | The LAN address used in Local invitations. |
| `--config <file>` | Use a different configuration file. |

## Files

- **First run:** Piik unpacks its helper programs into a `runtime` folder
  **next to `Piik.exe`** (`runtime\native`, `runtime\tunnel`). Later runs reuse
  them; a new `Piik.exe` replaces them automatically. If the folder is
  read-only, Piik prints a warning and the share features that need the helpers
  will not work, so keep the exe somewhere writable.
- **Settings, presets and remembered choices:** in a `piik-data` folder **next to
  `Piik.exe`**, so they move with the folder. The first time, anything already
  saved in `%APPDATA%\Piik` (including a saved Site address or access password,
  if you set one) is copied in once; after that the two are independent. Pass
  `--config <file>` to keep them somewhere else.
- **Diagnostics:** only with `--debug`, in the folder you choose.
- **Moving or removing Piik:** move or delete the whole folder (`Piik.exe`,
  `runtime` and `piik-data`). Nothing else is left behind, except the older
  `%APPDATA%\Piik` copy if you ever used a non-standalone build.

## Picture and encoder settings

- **Codecs:** H.264, H.265 and AV1 encode on the NVIDIA GPU's video encoder.
  VP8 is a CPU fallback (the GPU has no VP8 encoder); it is used only when a
  viewer cannot play the other codecs or hardware encoding fails.
- **Frame rate for phones:** the Android phone this was tested on (Chrome) could
  not decode much above 90 fps. For phone viewers use 60 fps; if you stream at
  100 fps or more, a phone viewer is held at half rate automatically.
- **Large pictures on phones:** a phone joining a full 2560-wide picture can
  freeze for about two seconds at the start. A 1080p picture avoids it.
- **While gaming:** the encoder is a separate part of the GPU from the one that
  draws the game, so it keeps up with the game running at full load. H.265 at
  2560x1600 and 120 fps uses most of the encoder, so H.264 or a lower frame rate
  gives more headroom if your GPU is under heavy load.

## Troubleshooting

- **Windows SmartScreen says the app is unrecognised.** The file is not
  code-signed. Choose More info, then Run anyway, if you trust where you got it.
- **Windows Firewall asks about Piik.** Allow it on private networks so viewers
  on your network can connect.
- **The page says "Not connected to server".** Reload it from the address Piik
  printed in the console.
- **Port 8787 is busy.** Close the other Piik, or use `--port`.
- **Viewer says the room cannot be joined by code.** Use the invitation link;
  joining by code is a room setting on the Host.
- **No hardware encoder found.** Update the NVIDIA driver, or choose another
  codec in the Host's settings.
- **Stuck Piik process.** Close its console window, or end `Piik.exe`,
  `piik-capture-bin.exe` and `cloudflared.exe` in Task Manager.

## Licences

Third-party licence notices ship with the helpers in `runtime\tunnel` and in the
web interface.
