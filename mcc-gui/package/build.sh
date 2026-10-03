#!/bin/sh
# Builds mcc-gui inside the images next to this script, the Makefile's
# qt-linux, qt-windows, qt-macos and qt-release run it:
#
#   build.sh linux amd64|arm64    dist/mcc-gui-linux-<arch>-<version>
#   build.sh windows amd64|arm64  dist/mcc-gui-windows-<arch>-<version>.exe
#   build.sh macos arm64|x86_64   dist/mcc-gui-darwin-amd64|arm64-<version>.app.zip
#
# With "package" after the arch, Linux builds a .tar.xz installing the binary,
# a desktop entry and the icon to /usr/local, and Windows a .zip of the exe.
# macOS always builds the .app, the binary doesn't run without its Qt.
#
# The repository is at /src, VERSION, COMMIT, DATE, APP_VERSION and
# APP_BUILD come from the Makefile.
set -eu
target=$1 arch=$2 package=${3:-}
cd /src
name="MeshCentral Client"
id=com.github.lexpaval.mcc-gui
ldflags="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$DATE"
mkdir -p dist
work=$(mktemp -d)

case $target in
linux)
	if [ "$arch" = arm64 ]; then
		export GOARCH=arm64 CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++
		export PKG_CONFIG_LIBDIR=/usr/lib/aarch64-linux-gnu/pkgconfig:/usr/share/pkgconfig
	fi
	bin="dist/mcc-gui-linux-$arch-$VERSION"
	go build -trimpath -ldflags "$ldflags" -o "$bin" ./mcc-gui
	[ "$package" = package ] || exit 0
	root=$work/usr/local
	install -Dm755 "$bin" "$root/bin/mcc-gui"
	rm "$bin"
	install -Dm644 mcc-gui/Icon.png "$root/share/icons/hicolor/512x512/apps/$id.png"
	install -d "$root/share/applications"
	cat > "$root/share/applications/$id.desktop" <<-EOF
		[Desktop Entry]
		Type=Application
		Name=$name
		Comment=Devices, port routes and shells on a MeshCentral server
		Exec=mcc-gui
		Icon=$id
		Categories=Network;RemoteAccess;
	EOF
	# Unpacks to /usr/local: sudo tar -xJf <file> -C /
	tar -C "$work" -cJf "dist/mcc-gui-linux-$arch-$VERSION.tar.xz" usr
	;;

windows)
	case $arch in
	amd64) env=/msys/clang64 cc=x86_64-w64-mingw32 ;;
	arm64) env=/msys/clangarm64 cc=aarch64-w64-mingw32 ;;
	esac
	export GOARCH=$arch CC=$cc-clang CXX=$cc-clang++ PKG_CONFIG_PATH=$env/qt6-static/lib/pkgconfig
	CGO_LDFLAGS=$(cat "$PKG_CONFIG_PATH/ldflags.txt")
	# miqt's gen_qrunnable.cpp uses std::nothrow, newer libc++ no longer
	# brings in <new> on the way.
	export CGO_LDFLAGS CGO_CXXFLAGS="-include new"
	# Exe icon, version info and manifest (DPI awareness, visual styles).
	GOOS= GOARCH= CGO_ENABLED=0 go tool -modfile=tools.mod go-winres simply --arch "$arch" --icon mcc-gui/Icon.png --manifest gui \
		--product-name "$name" --file-description "$name" --original-filename mcc-gui.exe \
		--product-version "$APP_VERSION.$APP_BUILD" --file-version "$APP_VERSION.$APP_BUILD" --out mcc-gui/rsrc
	trap 'rm -f mcc-gui/rsrc_windows_*.syso' EXIT
	exe="dist/mcc-gui-windows-$arch-$VERSION.exe"
	go build -trimpath -tags windowsqtstatic -ldflags "$ldflags -H windowsgui" -o "$exe" ./mcc-gui
	if [ "$package" = package ]; then
		cp "$exe" "$work/mcc-gui.exe"
		rm -f "${exe%.exe}.zip"
		(cd "$work" && zip -q "/src/${exe%.exe}.zip" mcc-gui.exe)
		rm "$exe"
	fi
	;;

macos)
	goarch=$arch
	[ "$arch" = x86_64 ] && goarch=amd64
	qt=/opt/local/libexec/qt6
	# Bundled under a name without the space, the walk below splits on spaces.
	app="$work/bundle.app"
	c="$app/Contents"
	mkdir -p "$c/MacOS" "$c/Frameworks" "$c/PlugIns/platforms" "$c/PlugIns/styles" "$c/Resources"
	GOARCH=$goarch go build -trimpath -ldflags "$ldflags" -o "$c/MacOS/mcc-gui" ./mcc-gui
	cp "$qt/plugins/platforms/libqcocoa.dylib" "$c/PlugIns/platforms/"
	cp "$qt/plugins/styles/libqmacstyle.dylib" "$c/PlugIns/styles/" 2>/dev/null || true
	printf '[Paths]\nPlugins = PlugIns\n' > "$c/Resources/qt.conf"

	# What macdeployqt does: copy every MacPorts library the app loads,
	# transitively, into Frameworks and point the references there.
	fw='@executable_path/../Frameworks'
	queue="$c/MacOS/mcc-gui $(find "$c/PlugIns" -name '*.dylib')"
	while [ -n "$queue" ]; do
		set -- $queue
		f=$1
		shift
		queue="$*"
		for dep in $(otool -L "$f" | tail -n +2 | awk '{print $1}' | grep '^/opt/local/'); do
			case $dep in
			*.framework/*)
				fwname=${dep%%.framework/*}
				fwname=${fwname##*/}
				rel="$fwname.framework/${dep#*.framework/}"
				if [ ! -d "$c/Frameworks/$fwname.framework" ]; then
					cp -R "${dep%%.framework/*}.framework" "$c/Frameworks/"
					rm -rf "$c/Frameworks/$fwname.framework/Headers" "$c/Frameworks/$fwname.framework"/Versions/*/Headers
					install_name_tool -id "$fw/$rel" "$c/Frameworks/$rel"
					queue="$queue $c/Frameworks/$rel"
				fi
				;;
			*)
				rel=${dep##*/}
				if [ ! -f "$c/Frameworks/$rel" ]; then
					cp -L "$dep" "$c/Frameworks/$rel"
					chmod u+w "$c/Frameworks/$rel"
					install_name_tool -id "$fw/$rel" "$c/Frameworks/$rel"
					queue="$queue $c/Frameworks/$rel"
				fi
				;;
			esac
			install_name_tool -change "$dep" "$fw/$rel" "$f"
		done
	done

	# An .icns holding the PNG as is, its size picks the entry type.
	python3 - mcc-gui/Icon.png "$c/Resources/icon.icns" <<-'EOF'
		import struct, sys
		png = open(sys.argv[1], 'rb').read()
		w = struct.unpack('>I', png[16:20])[0]
		kind = {128: b'ic07', 256: b'ic08', 512: b'ic09', 1024: b'ic10'}[w]
		entry = kind + struct.pack('>I', len(png) + 8) + png
		open(sys.argv[2], 'wb').write(b'icns' + struct.pack('>I', len(entry) + 8) + entry)
	EOF
	cat > "$c/Info.plist" <<-EOF
		<?xml version="1.0" encoding="UTF-8"?>
		<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
		<plist version="1.0">
		<dict>
			<key>CFBundleExecutable</key><string>mcc-gui</string>
			<key>CFBundleIdentifier</key><string>$id</string>
			<key>CFBundleName</key><string>$name</string>
			<key>CFBundleDisplayName</key><string>$name</string>
			<key>CFBundleIconFile</key><string>icon</string>
			<key>CFBundlePackageType</key><string>APPL</string>
			<key>CFBundleShortVersionString</key><string>$APP_VERSION</string>
			<key>CFBundleVersion</key><string>$APP_BUILD</string>
			<key>LSMinimumSystemVersion</key><string>14.0</string>
			<key>NSHighResolutionCapable</key><true/>
			<key>NSPrincipalClass</key><string>NSApplication</string>
		</dict>
		</plist>
	EOF
	# install_name_tool voids the signatures, Apple silicon runs nothing unsigned.
	mv "$app" "$work/$name.app"
	rcodesign sign "$work/$name.app" > /dev/null
	rm -f "dist/mcc-gui-darwin-$goarch-$VERSION.app.zip"
	(cd "$work" && zip -qry "/src/dist/mcc-gui-darwin-$goarch-$VERSION.app.zip" "$name.app")
	;;

*)
	echo "unknown target $target" >&2
	exit 1
	;;
esac
rm -rf "$work"
