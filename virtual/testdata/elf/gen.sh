#!/bin/sh
# gen.sh regenerates the synthetic ELF fixtures used by glibc_test.go. They
# are tiny x86-64 shared objects that imitate libc.so.6 (or not), built with
# the host gcc; nothing here is taken from a real C library.
set -eu
cd "$(dirname "$0")"

mk() { # mk <out> <soname> <version-script|-> <banner|->
	out=$1 soname=$2 script=$3 banner=$4
	{
		echo 'int fake_func(void) { return 0; }'
		echo 'int fake_func2(void) { return 1; }'
		if [ "$banner" != - ]; then
			printf 'const char fake_banner[] = "%s";\n' "$banner"
		fi
	} > lib.c
	set -- -shared -fPIC -nostdlib -O0 -o "$out" lib.c -Wl,-soname,"$soname" -Wl,--hash-style=gnu -s
	if [ "$script" != - ]; then
		set -- "$@" -Wl,--version-script="$script"
	fi
	gcc "$@"
	rm -f lib.c
}

cat > versions.map <<'MAP'
GLIBC_2.2.5 { global: fake_func; local: *; };
GLIBC_2.17 { global: fake_func2; } GLIBC_2.2.5;
GLIBC_2.28 { } GLIBC_2.17;
GLIBC_PRIVATE { } GLIBC_2.28;
MAP

mk libc-banner.so libc.so.6 versions.map 'GNU C Library (Fake) stable release version 2.29.\nCopyright'
mk libc-nobanner.so libc.so.6 versions.map -
mk libc-otherbanner.so libc.so.6 versions.map 'GNU C Library (Fake) stable release version 3.1.'
mk libc-oldbanner.so libc.so.6 versions.map 'GNU C Library (Fake) stable release version 2.17.'
mk wrong-soname.so libfoo.so.1 versions.map -
mk musl.so libc.musl-x86_64.so.1 - -
mk noversions.so libc.so.6 - -
rm -f versions.map
