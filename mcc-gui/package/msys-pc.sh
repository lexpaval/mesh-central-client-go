#!/bin/sh
# Writes pkg-config files for MSYS2's static Qt, which ships none, for miqt:
# Qt6Core, Qt6Gui and Qt6Widgets, whose static libraries (for miqt's
# windowsqtstatic tag) take in the Windows platform and style plugins and
# every library the .prl files list. Those go to ldflags.txt for CGO_LDFLAGS
# instead of Libs.private: cgo refuses the files among them (resource
# objects, archives by path) and pkgconf 3 drops some of the -l flags from a
# list this long. Run for /msys/clang64 and /msys/clangarm64.
set -eu
env=$1
prefix=$env/qt6-static
pc=$prefix/lib/pkgconfig
mkdir -p "$pc"

# The libraries a .prl file lists, with its placeholders resolved.
prl() {
	sed -n 's/^QMAKE_PRL_LIBS = //p' "$1" |
		sed -e "s#\$\$\[QT_INSTALL_PREFIX\]#$prefix#g" -e "s#\$\$\[QT_INSTALL_LIBS\]#$prefix/lib#g" \
			-e "s#\$\$\[QT_INSTALL_PLUGINS\]#$prefix/share/qt6/plugins#g"
}
all="$(prl "$prefix/lib/Qt6Widgets.prl") $(prl "$prefix/share/qt6/plugins/platforms/qwindows.prl") \
$(prl "$prefix/share/qt6/plugins/styles/qmodernwindowsstyle.prl")"
# Files first, then flags, deduplicated. lld doesn't mind the order.
{
	printf '%s\n' $all | grep -v '^-'
	printf '%s\n' -static "-L$env/lib" $all | grep '^-'
} | awk '!seen[$0]++' | tr '\n' ' ' > "$pc/ldflags.txt"

module() { # name, requires, cflags
	cat > "$pc/Qt6$1.pc" <<-EOF
		prefix=$prefix
		libdir=\${prefix}/lib
		includedir=\${prefix}/include/qt6

		Name: Qt6 $1
		Description: Qt $1 module
		Version: $(sed -n 's/.*define QT_VERSION_STR "\(.*\)"/\1/p' "$prefix/include/qt6/QtCore/qtversion.h")
		Requires: $2
		Libs: -L\${libdir} -lQt6$1
		Cflags: -I\${includedir}/Qt$1 -I\${includedir} $3
	EOF
}
module Platform "" "-I$prefix/share/qt6/mkspecs/win32-clang-g++ -DUNICODE -D_UNICODE -DWIN32 -DWIN64 -D_WIN64"
sed -i '/^Libs/d' "$pc/Qt6Platform.pc"
module Core Qt6Platform -DQT_CORE_LIB
module Gui Qt6Core -DQT_GUI_LIB
module Widgets "Qt6Core Qt6Gui" -DQT_WIDGETS_LIB
