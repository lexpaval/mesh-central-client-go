#!/bin/sh
# Bundle without executing target binaries, so arm64 stays a cross-build.
set -eu
bin=$1 arch=$2 app=$3
case $arch in
amd64) triplet=x86_64-linux-gnu runtime=x86_64 ;;
arm64) triplet=aarch64-linux-gnu runtime=aarch64 ;;
*) echo "unsupported AppImage architecture: $arch" >&2; exit 1 ;;
esac
id=com.github.lexpaval.mcc-gui
qt=/opt/qt/$arch
install -Dm755 "$bin" "$app/usr/bin/mcc-gui"
install -Dm644 mcc-gui/Icon.png "$app/$id.png"
install -Dm644 LICENSE "$app/usr/share/doc/mcc-gui/copyright"
cat > "$app/$id.desktop" <<-EOF
	[Desktop Entry]
	Type=Application
	Name=MeshCentral Client
	Comment=Devices, port routes and shells on a MeshCentral server
	Exec=mcc-gui
	Icon=$id
	Categories=Network;RemoteAccess;
EOF
ln -s "$id.png" "$app/.DirIcon"
printf '[Paths]\nPrefix = ..\nPlugins = plugins\n' > "$app/usr/bin/qt.conf"

# Qt loads these at runtime; ELF dependencies alone cannot discover them.
mkdir -p "$app/usr/plugins/platforms"
for platform in xcb wayland offscreen; do
	cp "$qt/plugins/platforms/libq$platform.so" "$app/usr/plugins/platforms/"
done

# GTK uses this module to read GNOME's theme and font preferences on Wayland.
install -Dm644 "/usr/lib/$triplet/gio/modules/libdconfsettings.so" "$app/usr/lib/gio/modules/libdconfsettings.so"
for category in imageformats platforminputcontexts platformthemes xcbglintegrations wayland-*; do
	for dir in "$qt/plugins"/$category; do
		[ ! -d "$dir" ] || cp -R "$dir" "$app/usr/plugins/"
	done
done

# lddtree reads ELF metadata and resolves libraries for the target architecture.
find "$app/usr" -type f \( -name '*.so' -o -name mcc-gui \) > "$app/elf-files"
: > "$app/dependencies"
while IFS= read -r elf; do
	LD_LIBRARY_PATH="$qt/lib" lddtree -l "$elf" >> "$app/dependencies"
done < "$app/elf-files"
sort -u -o "$app/dependencies" "$app/dependencies"
mkdir -p "$app/usr/lib" "$app/usr/share/doc/libraries"
cp -R /usr/share/common-licenses "$app/usr/share/doc/"
: > "$app/usr/share/doc/libraries/versions.txt"
while IFS= read -r lib; do
	case $lib in
	"$app"/*) continue ;;
	# Keep the host's libc and loader together. Hardware drivers remain on the host.
	*/ld-linux*|*/libc.so.*|*/libm.so.*|*/libpthread.so.*|*/libdl.so.*|*/librt.so.*|*/libresolv.so.*) continue ;;
	esac
	[ -f "$lib" ] || { echo "unresolved dependency: $lib" >&2; exit 1; }
	cp -L "$lib" "$app/usr/lib/"
	case $lib in /opt/qt/*) continue ;; esac
	# Debian records some merged-/usr libraries under their original /lib path.
	real=$(readlink -f "$lib")
	owner=$(dpkg-query -S "$real" 2>/dev/null || dpkg-query -S "${real#/usr}" 2>/dev/null)
	owner=${owner%%: /*}
	install -Dm644 "/usr/share/doc/${owner%:*}/copyright" "$app/usr/share/doc/libraries/$owner"
	dpkg-query -W -f='${binary:Package} ${Version}\n' "$owner" >> "$app/usr/share/doc/libraries/versions.txt"
done < "$app/dependencies"
sort -u -o "$app/usr/share/doc/libraries/versions.txt" "$app/usr/share/doc/libraries/versions.txt"
qtversion=$(PKG_CONFIG_PATH="$qt/lib/pkgconfig" pkg-config --modversion Qt6Widgets)
printf 'Qt %s\nSources: https://download.qt.io/archive/qt/%s/%s/submodules/\nQt Base, Qt Wayland and Qt SVG use LGPL-3.0 (see ../common-licenses/LGPL-3).\n' \
	"$qtversion" "${qtversion%.*}" "$qtversion" > "$app/usr/share/doc/libraries/Qt.txt"
rm "$app/elf-files" "$app/dependencies"

# RPATH avoids leaking LD_LIBRARY_PATH into browsers and SSH/RDP clients.
patchelf --set-rpath '$ORIGIN/../lib' "$app/usr/bin/mcc-gui"
for lib in "$app"/usr/lib/*; do
	[ -f "$lib" ] || continue
	patchelf --set-rpath '$ORIGIN' "$lib"
done
find "$app/usr/plugins" -name '*.so' -exec patchelf --set-rpath '$ORIGIN/../../lib' {} \;
patchelf --set-rpath '$ORIGIN/../..' "$app/usr/lib/gio/modules/libdconfsettings.so"

mkdir -p "$app/usr/share/fonts" "$app/usr/etc/fonts" "$app/usr/share/X11"
cp /usr/share/fonts/truetype/dejavu/DejaVuSans.ttf /usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf "$app/usr/share/fonts/"
cp -R /usr/share/X11/xkb "$app/usr/share/X11/"
cp /usr/share/doc/fonts-dejavu-core/copyright "$app/usr/share/doc/libraries/fonts-dejavu-core"
cp /usr/share/doc/xkb-data/copyright "$app/usr/share/doc/libraries/xkb-data"
cp /usr/share/doc/dconf-gsettings-backend/copyright "$app/usr/share/doc/libraries/dconf-gsettings-backend"
cp /etc/fonts/fonts.conf "$app/usr/etc/fonts/"
cp -LR /etc/fonts/conf.d "$app/usr/etc/fonts/"
sed -i '/<\/fontconfig>/i\  <dir prefix="relative">../../share/fonts</dir>' "$app/usr/etc/fonts/fonts.conf"
cat > "$app/AppRun" <<-'EOF'
	#!/bin/sh
	set -eu
	app=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
	# Match the bundled fontconfig version while retaining its system/user font paths.
	if [ -z "${FONTCONFIG_FILE:-}" ] && [ -z "${FONTCONFIG_PATH:-}" ]; then
		export FONTCONFIG_FILE="$app/usr/etc/fonts/fonts.conf"
		export FONTCONFIG_PATH="$app/usr/etc/fonts"
	fi
	export GIO_EXTRA_MODULES="$app/usr/lib/gio/modules${GIO_EXTRA_MODULES:+:$GIO_EXTRA_MODULES}"
	if [ ! -d /usr/share/X11/xkb ] && [ -z "${XKB_CONFIG_ROOT:-}" ]; then
		export XKB_CONFIG_ROOT="$app/usr/share/X11/xkb"
	fi
	exec "$app/usr/bin/mcc-gui" "$@"
EOF
chmod +x "$app/AppRun"
output=$(mktemp "$bin.AppImage.XXXXXX")
trap 'rm -f "$output"' EXIT
ARCH=$runtime /opt/appimage/squashfs-root/AppRun --no-appstream \
	--runtime-file "/opt/appimage/runtime-$runtime" "$app" "$output"
mv "$output" "$bin.AppImage"
