package codeserver

import "strconv"

// The Chinese language pack for VS Code (MIT, published on Open VSX). It is
// installed into code-server's extensions on a host when a user with the
// Chinese interface opens VS Code there, and code-server is then started
// with that locale. Like code-server's own releases it is never shipped with
// Corral: the host downloads it, or the gateway uploads it from
// CORRAL_CODE_SERVER_DIR; either way it is checked against the pinned digest.
const (
	LangPackID       = "ms-ceintl.vscode-language-pack-zh-hans" // its folder name in the extensions, before "-<version>"
	LangPackVersion  = "1.131.0"
	LangPackSHA256   = "7ffc9dbe980f95942ed56752c1d6ca5f40855013aedd34cf40257929be4d1895"
	LangPackFile     = "MS-CEINTL.vscode-language-pack-zh-hans-" + LangPackVersion + ".vsix"
	LangPackURL      = "https://open-vsx.org/api/MS-CEINTL/vscode-language-pack-zh-hans/" + LangPackVersion + "/file/" + LangPackFile
	LangPackLocale   = "zh-cn"
	langPackMaxBytes = 32 << 20
)

// LangPackSeconds bounds the download on the host, langPackInstallSeconds
// the installation and langPackLock the wait for the host's lock: together
// well inside the gateway's step limit, so the command (and the lock it
// holds) has ended when the step does.
var LangPackSeconds = 50

var langPackInstallSeconds = 40

// langPackLock is lock with a short wait: a host busy with something else
// just gets VS Code in English this time.
const langPackLock = `mkdir -p "$ST" && chmod 700 "$ST" || { echo error=mkdir; exit 1; }
command -v flock >/dev/null 2>&1 || { echo error=noflock; exit 1; }
command -v setsid >/dev/null 2>&1 || { echo error=nosetsid; exit 1; }
exec 9>"$ST/.lock" && flock -w 10 9 || { echo error=locked; exit 1; }
`

// langPackSum is the digest checked (LangPackSHA256; tests use their own pack).
var langPackSum = LangPackSHA256

// langPackRegister checks the installed pack and records it in
// code-server's languagepacks.json, as VS Code itself does once a browser has
// connected (without it the first opening is in English). It mirrors VS Code
// 1.138's own entry: per language of the pack, its translation files and a
// hash of the extension and version; other languages' entries are kept. It
// fails (and changes nothing) unless the pack has the locale and all of its
// translation files (each a JSON object with "contents"), and when the existing file cannot be read as a record.
// Arguments: user data directory, extensions directory, extension id,
// version, locale, and "check" to only check the pack. Run with
// code-server's own node.
const langPackRegister = `const fs = require('fs'), path = require('path'), crypto = require('crypto')
const [dataDir, extDir, id, version, locale, mode] = process.argv.slice(1)
const dir = path.join(extDir, id + '-' + version)
const manifest = JSON.parse(fs.readFileSync(path.join(dir, 'package.json'), 'utf8'))
const usable = (f) => { const j = JSON.parse(fs.readFileSync(f, 'utf8')); return !!j && !!j.contents && typeof j.contents === 'object' && !Array.isArray(j.contents) }
const locs = (manifest.contributes && manifest.contributes.localizations) || []
const ok = (l) => l && typeof l.languageId === 'string' && Array.isArray(l.translations) && l.translations.length > 0 &&
  l.translations.every((t) => t && typeof t.id === 'string' && typeof t.path === 'string' && usable(path.join(dir, t.path)))
if (!locs.every(ok) || !locs.some((l) => l.languageId.toLowerCase() === locale)) process.exit(1)
if (mode === 'check') process.exit(0)
const file = path.join(dataDir, 'languagepacks.json')
let packs = {}
try { packs = JSON.parse(fs.readFileSync(file, 'utf8')) } catch (e) { if (e.code !== 'ENOENT') process.exit(1) }
if (!packs || typeof packs !== 'object' || Array.isArray(packs)) process.exit(1)
for (const loc of locs) {
  const p = { hash: '', extensions: [{ extensionIdentifier: { id }, version: manifest.version }], translations: {}, label: loc.localizedLanguageName || loc.languageName }
  for (const t of loc.translations) p.translations[t.id] = path.join(dir, t.path)
  p.hash = crypto.createHash('md5').update(id).update(manifest.version).digest('hex')
  packs[loc.languageId] = p
}
fs.writeFileSync(file + '.new', JSON.stringify(packs))
fs.renameSync(file + '.new', file)
`

// langPackUsable is true when the pack is installed whole. It needs $D
// (code-server's folder) and $LPJS (langPackRegister).
const langPackUsable = `[ -d "$DATA/extensions/$LPID-$LPVER" ] &&
  "$D/lib/node" -e "$LPJS" "$DATA" "$DATA/extensions" "$LPID" "$LPVER" "$LPLOC" check >/dev/null 2>&1 9>&- </dev/null`

// LangPackCommand installs the language pack for code-server version
// (installed already), unless it is there. With from FromStdin the pack is
// read from the command's input, otherwise the host downloads url. It holds
// the host's lock (installs, starts and stops wait) and works in a staging
// directory of its own; nothing is installed unless the digest matches.
// (Reading the pack from the input waits for the gateway, which ends the
// command by closing its input should the step take too long.)
func LangPackCommand(version, from, url string) string {
	return script(layout+langPackLock+`umask 077
D="$L/code-server-$V"
X="$DATA/extensions"
drain() { [ "$M" = stdin ] && cat >/dev/null; }
[ -x "$D/bin/code-server" ] || { drain; echo error=notinstalled; exit 1; }
if `+langPackUsable+`; then drain; echo result=ok; exit 0; fi
mkdir -p "$X" && chmod 700 "$HOME/.local/share/corral" "$DATA" || { drain; echo error=mkdir; exit 1; }
STAGE=$(mktemp -d "$ST/langpack-XXXXXX") || { drain; echo error=stage; exit 1; }
trap 'rm -rf "$STAGE"' EXIT
F="$STAGE/pack.vsix"
if [ "$M" = stdin ]; then
  head -c "$MAXB" >"$F" || { echo error=upload; exit 1; }
  cat >/dev/null
else
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 20 --max-time "$MAXT" --max-filesize "$MAXB" \
      -o "$F" "$U" 2>"$STAGE/err"; rc=$?
  elif command -v wget >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
    timeout "$MAXT" wget -q --https-only -T 20 -t 2 -O "$F" "$U" 2>"$STAGE/err"; rc=$?
  else
    echo error=nodownloader; exit 1
  fi
  if [ "$rc" != 0 ]; then echo error=download; echo "detail=$(tail -c 300 "$STAGE/err" 2>/dev/null | tr '\n' ' ')"; exit 1; fi
fi
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum <"$F") || { echo error=nosha; exit 1; }
elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 <"$F") || { echo error=nosha; exit 1; }
else echo error=nosha; exit 1; fi
got=${got%% *}
[ "$got" = "$H" ] || { echo error=checksum; echo "detail=$got"; exit 1; }
# A damaged copy is uninstalled first: VS Code installs over it only then.
# This runs in a process group of its own (in the background of this shell
# it is no group leader, so setsid need not fork), whose leader notes the
# result and then ends the whole group, its watcher included; the watcher
# ends it the same way once its time is up. The group is only ever
# signalled from inside, while it exists. (Nothing in it keeps the lock,
# nor the command's output open.)
rm -f "$STAGE/rc"
setsid sh -c '( sleep "$7"; kill -KILL 0 ) </dev/null >/dev/null 2>&1 &
  if [ -d "$3/$4-$5" ] || grep -qF "$4" "$3/extensions.json" 2>/dev/null; then
    "$1" --user-data-dir "$2" --extensions-dir "$3" --uninstall-extension "$4" || :
  fi
  "$1" --user-data-dir "$2" --extensions-dir "$3" --install-extension "$6"
  echo "$?" >"$8"
  kill -KILL 0' \
  corral-langpack "$D/bin/code-server" "$DATA" "$X" "$LPID" "$LPVER" "$F" "$MAXI" "$STAGE/rc" >"$STAGE/out" 2>&1 </dev/null 9>&- &
wait "$!" 2>/dev/null # (without the shell's note that it was killed)
rc=$(cat "$STAGE/rc" 2>/dev/null) || rc=timeout
[ "$rc" = 0 ] || { echo error=install; echo "detail=($rc) $(tail -c 300 "$STAGE/out" 2>/dev/null | tr '\n' ' ')"; exit 1; }
`+langPackUsable+` || { echo error=install; echo "detail=$(tail -c 300 "$STAGE/out" 2>/dev/null | tr '\n' ' ')"; exit 1; }
echo result=ok
`, "V", version, "M", from, "U", url, "H", langPackSum, "LPID", LangPackID, "LPVER", LangPackVersion,
		"LPLOC", LangPackLocale, "LPJS", langPackRegister,
		"MAXT", strconv.Itoa(LangPackSeconds), "MAXI", strconv.Itoa(langPackInstallSeconds), "MAXB", strconv.Itoa(langPackMaxBytes))
}
