#!/usr/bin/env bash
# 验证输入为真实 ELF 静态库时，构建脚本能清理全部 LLVM 段并保留可链接的函数。
# 使用本地 cargo 替身复制预制静态库，覆盖成功、无段、失败和非 Linux 路径；不下载 Rust 依赖。
# 用法：bash scripts/test-anydoc-lto.sh（Linux，需 gcc、ar、readelf、objcopy、nm）。
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
for tool in gcc ar readelf objcopy nm; do
  command -v "$tool" >/dev/null || { echo "missing test tool: $tool" >&2; exit 1; }
done
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/repo/scripts" "$fixture/bin" "$fixture/repo/third_party/anydoc-go/patched-anydoc"
cp "$repo_root/scripts/build-anydoc-lib.sh" "$fixture/repo/scripts/"
crate="$fixture/repo/third_party/anydoc-go"
printf 'anydoc = "=0.1.9"\n' > "$crate/Cargo.toml"
printf 'const Version = "0.1.9"\n' > "$crate/version.go"
printf '0.1.9\n' > "$crate/patched-anydoc/.weknora-patched"
touch "$crate/patched-anydoc/Cargo.toml"
cat > "$fixture/bin/cargo" <<'CARGO'
#!/usr/bin/env bash
set -euo pipefail
while [ "$1" != '--manifest-path' ]; do shift; done
crate=$(dirname "$2")
mkdir -p "$crate/target/$TARGET/release"
cp "$ARCHIVE_FIXTURE" "$crate/target/$TARGET/release/libanydoc_go.a"
CARGO
chmod +x "$fixture/bin/cargo"
export PATH="$fixture/bin:$PATH"
export TARGET=x86_64-unknown-linux-gnu
export ARCHIVE_FIXTURE="$fixture/clean.a"
printf 'int anydoc_fixture(void) { return 42; }\n' > "$fixture/library.c"
gcc -c "$fixture/library.c" -o "$fixture/library.o"
ar rcs "$fixture/clean.a" "$fixture/library.o"
printf 'int anydoc_fixture(void); int main(void) { return anydoc_fixture() == 42 ? 0 : 1; }\n' > "$fixture/main.c"

# build_fixture 无参数；调用完整构建脚本并输出日志，失败状态原样返回。
build_fixture() {
  bash "$fixture/repo/scripts/build-anydoc-lib.sh" > "$fixture/build.log" 2>&1
}
# expect_failure 无参数；验证清理失败不会被吞掉或误报构建成功。
expect_failure() {
  if build_fixture; then
    echo 'expected archive cleanup to fail' >&2
    exit 1
  fi
  if grep -q '^Wrote ' "$fixture/build.log"; then
    echo 'cleanup failure was incorrectly reported as success' >&2
    exit 1
  fi
}
output="$crate/lib/linux_amd64_gnu/libanydoc_go.a"

# 无 LLVM 段时输出副本与输入完全一致。
build_fixture
cmp "$ARCHIVE_FIXTURE" "$output"

# 在真实对象文件中添加多种元数据段，包括带下划线和点的名称，确认全部清理且函数仍可链接执行。
printf 'fixture LLVM metadata\n' > "$fixture/metadata"
cp "$fixture/library.o" "$fixture/with-llvm.o"
for section in .llvmbc .llvmcmd .llvm_addrsig .llvm.extra; do
  objcopy --add-section="$section=$fixture/metadata" "$fixture/with-llvm.o"
done
ar rcs "$fixture/with-llvm.a" "$fixture/with-llvm.o"
export ARCHIVE_FIXTURE="$fixture/with-llvm.a"
build_fixture
if readelf -SW "$output" | grep -q '\.llvm'; then
  echo 'LLVM sections remain' >&2
  exit 1
fi
nm -g "$output" | grep -q 'anydoc_fixture'
gcc "$fixture/main.c" "$output" -o "$fixture/check-link"
"$fixture/check-link"
# 清理只作用于副本，不破坏 cargo 生成的原始库。
cmp "$ARCHIVE_FIXTURE" "$crate/target/$TARGET/release/libanydoc_go.a"

# objcopy 报错和假成功（不删除段）都必须终止构建。
printf '#!/usr/bin/env bash\nexit 1\n' > "$fixture/bin/objcopy"
chmod +x "$fixture/bin/objcopy"
expect_failure
grep -q 'could not remove' "$fixture/build.log"
printf '#!/usr/bin/env bash\nexit 0\n' > "$fixture/bin/objcopy"
expect_failure
grep -q 'LLVM sections remain' "$fixture/build.log"

# 非 ELF 平台不能执行 Linux 清理工具；只验证路由，不模拟 macOS 的实际链接。
printf '#!/usr/bin/env bash\nexit 1\n' > "$fixture/bin/readelf"
chmod +x "$fixture/bin/readelf"
export TARGET=aarch64-apple-darwin
build_fixture
cmp "$ARCHIVE_FIXTURE" "$crate/lib/darwin_arm64/libanydoc_go.a"
echo 'PASS: AnyDoc LLVM cleanup, symbol/link preservation, failure paths, and non-Linux routing'
