#!/usr/bin/env bash
# build-mcp.sh — knowledge-mcp 跨平台构建与打包（本地与 CI 共用的单一构建真相源）
#
# 引入动机：knowledge-mcp 需要以 6 个平台（windows/linux/darwin × amd64/arm64）分发。
# 若本地 Makefile 与 CI workflow 各自维护一份构建命令，ldflags 注入、产物命名、
# 校验和生成必然随时间漂移。本脚本是唯一的构建逻辑，两边都调用它。
#
# 用法：
#   scripts/build-mcp.sh [version] [outdir] [platforms]
#
#   参数：
#     version    注入 buildinfo.Version 的版本号。缺省时用
#                `git describe --tags --always --dirty` 推断；git 不可用则回落 "dev"。
#     outdir     产物输出目录。缺省 dist（相对路径以仓库根为基准）。
#     platforms  仅构建指定平台，格式 "os/arch" 或 "os/arch,os/arch"。
#                缺省为空 = 构建全部 6 个平台。也可用环境变量 PLATFORMS 指定，
#                第三参数优先级高于 PLATFORMS。
#
#   环境变量：
#     PLATFORMS  同第三参数（第三参数优先）。
#
# 产物（均位于 outdir）：
#   knowledge-mcp_<version>_<os>_<arch>.zip      windows 平台包，内含 knowledge-mcp.exe
#   knowledge-mcp_<version>_<os>_<arch>.tar.gz   其余平台包，内含 knowledge-mcp
#   <包名>.sha256                                单包校验和（内容为 "<hash>  <包名>"）
#   checksums.txt                                本次构建全部包的汇总校验和
#
# 依赖：go、tar+gzip、sha256sum；windows 包需要 zip 或 python3/python 之一。
#
# 退出码：
#   0 = 全部目标平台构建并打包成功（含校验和复验）
#   1 = 任一环节失败。任一步失败立即退出，不跳过失败平台。
#
# 安全/一致性：
#   - 构建使用 -trimpath 且 CGO_ENABLED=0，产物为静态可分发二进制。
#   - 打包后立即用 sha256sum -c 复验 checksums.txt，失败即报错退出。
set -euo pipefail

# --- 日志与失败处理 -----------------------------------------------------------

# log：输出进度日志到 stderr，避免污染 stdout。
log() { printf '[build-mcp] %s\n' "$*" >&2; }

# die：Fail Fast 终止，用于不可恢复的错误。
die() {
	printf '[build-mcp] ERROR: %s\n' "$*" >&2
	exit 1
}

# --- 环境与路径解析 -----------------------------------------------------------

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)
MCP_DIR="${REPO_ROOT}/mcp"

[ -d "${MCP_DIR}" ] || die "未找到 MCP 模块目录: ${MCP_DIR}"
command -v go >/dev/null 2>&1 || die "未找到 go，无法构建"
command -v sha256sum >/dev/null 2>&1 || die "未找到 sha256sum，无法生成校验和"
command -v tar >/dev/null 2>&1 || die "未找到 tar，无法打包"

# --- 参数解析 -----------------------------------------------------------------

# VERSION：第三优先级为参数，其次 git 推断，最后 dev。
VERSION="${1:-}"
if [ -z "${VERSION}" ]; then
	if command -v git >/dev/null 2>&1 &&
		VERSION=$(git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null) &&
		[ -n "${VERSION}" ]; then
		log "未指定版本，使用 git describe 推断: ${VERSION}"
	else
		VERSION=dev
		log "未指定版本且 git 不可用，回落版本: ${VERSION}"
	fi
fi

OUTDIR="${2:-dist}"
case "${OUTDIR}" in
/* | [A-Za-z]:[\\/]*) OUTDIR_ABS="${OUTDIR}" ;;
*) OUTDIR_ABS="${REPO_ROOT}/${OUTDIR}" ;;
esac

ALL_PLATFORMS="windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"
PLATFORM_SPEC="${3:-${PLATFORMS:-}}"
if [ -z "${PLATFORM_SPEC}" ]; then
	PLATFORM_SPEC="${ALL_PLATFORMS}"
else
	log "按指定平台构建: ${PLATFORM_SPEC}"
fi
# 允许逗号分隔，统一转成空格分隔的列表。
PLATFORM_SPEC="${PLATFORM_SPEC//,/ }"

# 先校验平台参数合法，再产生任何文件系统副作用（不留半成品 outdir）。
for platform in ${PLATFORM_SPEC}; do
	case "${platform}" in
	windows/amd64 | windows/arm64 | linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64) ;;
	*) die "不支持的平台: ${platform}（仅支持: ${ALL_PLATFORMS}）" ;;
	esac
done

# 版本号会进入产物文件名，提前拒绝含路径分隔符的非法值。
case "${VERSION}" in
*/* | *\\*) die "版本号不能包含路径分隔符: ${VERSION}" ;;
esac

# --- 构建元信息 ---------------------------------------------------------------

# COMMIT：git 不可达时使用 unknown（CI 之外的源码包场景）。
COMMIT=$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null) || COMMIT=""
if [ -z "${COMMIT}" ]; then
	COMMIT=unknown
	log "警告: git 不可用，Commit 回落为 ${COMMIT}"
fi

# DATE：UTC 构建时间，统一 RFC3339（秒级）。
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) || die "无法获取 UTC 时间"

LDFLAGS="-s -w -X partitura/mcp/internal/buildinfo.Version=${VERSION} -X partitura/mcp/internal/buildinfo.Commit=${COMMIT} -X partitura/mcp/internal/buildinfo.Date=${DATE}"

# --- 打包工具探测 -------------------------------------------------------------

# zip 工具：Linux（含 CI runner）自带 zip；Git Bash（MSYS2）通常没有 zip，
# 此时使用 python3/python 的 zipfile 模块（两者都能正确接收 MSYS 转换后的路径）。
# 候选解释器必须实际可导入 zipfile 才被采用（Windows 上 python3 可能是应用商店占位符）。
ZIP_TOOL=""
if command -v zip >/dev/null 2>&1; then
	ZIP_TOOL="zip"
else
	for candidate in python3 python; do
		if command -v "${candidate}" >/dev/null 2>&1 &&
			"${candidate}" -c 'import zipfile' >/dev/null 2>&1; then
			ZIP_TOOL="${candidate}"
			break
		fi
	done
fi

# windows 包必须用 zip；提前校验，避免构建到一半才失败。
case " ${PLATFORM_SPEC} " in
*" windows/"*)
	[ -n "${ZIP_TOOL}" ] ||
		die "windows 平台打包需要 zip 或可用的 python3/python，当前均不可用"
	;;
esac

# create_zip <workdir> <entry> <outfile>
# 在 <workdir> 下把单文件 <entry> 打包为 <outfile>（zip，条目名即 <entry>）。
create_zip() {
	local workdir="$1" entry="$2" out="$3"
	rm -f "${out}"
	case "${ZIP_TOOL}" in
	zip)
		(cd "${workdir}" && zip -q -X "${out}" "${entry}") ||
			die "zip 打包失败: ${out}"
		;;
	python3 | python)
		"${ZIP_TOOL}" - "${workdir}" "${entry}" "${out}" <<'PY' || die "python zipfile 打包失败: ${out}"
import os
import sys
import zipfile

src_dir, entry, out = sys.argv[1], sys.argv[2], sys.argv[3]
src = os.path.join(src_dir, entry)
if not os.path.isfile(src):
    sys.exit(f"missing input file: {src}")
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
    zf.write(src, arcname=entry)
PY
		;;
	*)
		die "缺少 zip 打包工具：请安装 zip，或提供 python3/python"
		;;
	esac
	[ -f "${out}" ] || die "zip 打包未生成文件: ${out}"
}

# --- 构建主流程 ---------------------------------------------------------------

if [ -n "${ZIP_TOOL}" ] && [ "${ZIP_TOOL}" != "zip" ]; then
	log "本机未找到 zip，windows 包改用 ${ZIP_TOOL} 生成"
fi

mkdir -p "${OUTDIR_ABS}" || die "无法创建输出目录: ${OUTDIR_ABS}"

WORK_DIR=$(mktemp -d) || die "无法创建临时目录"
trap 'rm -rf "${WORK_DIR}"' EXIT

log "仓库根: ${REPO_ROOT}"
log "版本: ${VERSION}  提交: ${COMMIT}  时间: ${DATE}"
log "输出目录: ${OUTDIR_ABS}"
log "Go: $(go version)"

BUILT_PKGS=()

for platform in ${PLATFORM_SPEC}; do
	goos="${platform%%/*}"
	goarch="${platform##*/}"
	bin_name="knowledge-mcp"
	[ "${goos}" = "windows" ] && bin_name="knowledge-mcp.exe"
	pkg_name="knowledge-mcp_${VERSION}_${goos}_${goarch}"

	stage_dir="${WORK_DIR}/${pkg_name}"
	mkdir -p "${stage_dir}" || die "无法创建暂存目录: ${stage_dir}"

	log "构建 ${platform} → ${bin_name}"
	# 在 mcp/ 模块目录内构建，输出到暂存目录；-o 使用绝对路径，MSYS 会转换为原生路径。
	(cd "${MCP_DIR}" &&
		CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
			go build -trimpath -ldflags "${LDFLAGS}" \
			-o "${stage_dir}/${bin_name}" ./cmd/knowledge-mcp) ||
		die "构建失败: ${platform}"

	[ -f "${stage_dir}/${bin_name}" ] || die "构建未产生二进制: ${stage_dir}/${bin_name}"

	if [ "${goos}" = "windows" ]; then
		pkg_ext="zip"
		pkg_file="${OUTDIR_ABS}/${pkg_name}.${pkg_ext}"
		# 条目名必须是包内文件名（knowledge-mcp.exe），不是暂存目录里的完整路径。
		create_zip "${stage_dir}" "${bin_name}" "${pkg_file}"
	else
		pkg_ext="tar.gz"
		pkg_file="${OUTDIR_ABS}/${pkg_name}.${pkg_ext}"
		rm -f "${pkg_file}"
		tar -czf "${pkg_file}" -C "${stage_dir}" "${bin_name}" ||
			die "tar 打包失败: ${pkg_file}"
	fi

	# 校验和记录相对文件名，使 outdir 内可直接 `sha256sum -c` 复验。
	pkg_base="${pkg_name}.${pkg_ext}"
	(cd "${OUTDIR_ABS}" && sha256sum "${pkg_base}" >"${pkg_base}.sha256") ||
		die "生成校验和失败: ${pkg_file}"

	BUILT_PKGS+=("${pkg_base}")
	log "完成 ${platform}: $(basename "${pkg_file}") ($(wc -c <"${pkg_file}") 字节)"
done

# --- 汇总校验和 ---------------------------------------------------------------

CHECKSUMS="${OUTDIR_ABS}/checksums.txt"
: >"${CHECKSUMS}" || die "无法写入 ${CHECKSUMS}"
for pkg_base in "${BUILT_PKGS[@]}"; do
	# 汇总本次构建的所有包校验和（单包 .sha256 文件内容格式与 sha256sum 一致）。
	cat "${OUTDIR_ABS}/${pkg_base}.sha256" >>"${CHECKSUMS}" ||
		die "汇总校验和失败: ${pkg_base}"
done

log "复验 ${CHECKSUMS}"
(cd "${OUTDIR_ABS}" && sha256sum -c checksums.txt) || die "校验和复验失败: ${CHECKSUMS}"

log "构建完成，产物清单:"
ls -l "${OUTDIR_ABS}" >&2
log "共 ${#BUILT_PKGS[@]} 个平台包 + checksums.txt"
