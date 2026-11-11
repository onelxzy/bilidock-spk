#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""BiliDock SPK 构建脚本。

流程：交叉编译 Go 服务 -> 准备图标 -> 组装 package.tgz 与外层 SPK。
产物：dist/BiliDock_<version>_<arch>.spk
"""

import json
import os
import shutil
import subprocess
import sys
import tarfile
import urllib.request
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / "tools"))
import spk_builder  # noqa: E402

PKG_ID = "bilidock"
TEMPLATE = ROOT / "package"
DIST = ROOT / "dist"
CACHE = ROOT / "cache"

GO_BIN = os.environ.get("GO_BIN") or shutil.which("go") or "go"
GOROOT = os.environ.get("GOROOT", "")
GOPATH = os.environ.get("GOPATH", "")

ARCHES = {
    "x86_64": "amd64",
    "armv8": "arm64",
}

FFMPEG_URLS = {
    "amd64": "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz",
    "arm64": "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-arm64-static.tar.xz",
}

FFMPEG_CACHE = {
    "amd64": CACHE / "ffmpeg-amd64.tar.xz",
    "arm64": CACHE / "ffmpeg-arm64.tar.xz",
}

MASTER_ICON = ROOT / "assets" / "icon-master.png"


def normalize_lf() -> None:
    """确保包内脚本、配置与 UI 定义统一为 LF 换行。"""
    targets = list((TEMPLATE / "scripts").glob("*")) + list((TEMPLATE / "conf").glob("*")) + [
        TEMPLATE / "target" / "ui" / "config",
        TEMPLATE / "target" / "ui" / "Main.js",
    ]
    for p in targets:
        if p.is_file():
            raw = p.read_bytes().replace(b"\r\n", b"\n")
            p.write_bytes(raw)


def sync_ui_version(version: str) -> None:
    cfg_path = TEMPLATE / "target" / "ui" / "config"
    data = json.loads(cfg_path.read_text(encoding="utf-8"))
    for item in data.get("Main.js", {}).values():
        if item.get("type") == "app":
            item["version"] = version
    cfg_path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")


def build_backend(goarch: str, version: str) -> Path:
    out = DIST / f"bilidock-linux-{goarch}"
    DIST.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env.update(
        {
            "GOOS": "linux",
            "GOARCH": goarch,
            "CGO_ENABLED": "0",
            "GOFLAGS": "-mod=mod",
        }
    )
    if GOROOT:
        env["GOROOT"] = GOROOT
        env["PATH"] = str(Path(GOROOT) / "bin") + os.pathsep + env.get("PATH", "")
    if GOPATH:
        env["GOPATH"] = GOPATH
    print(f"  编译 linux/{goarch} ...")
    subprocess.run(
        [GO_BIN, "build", "-trimpath", "-ldflags", f"-s -w -X main.Version={version}", "-o", str(out), "."],
        cwd=str(ROOT / "backend"),
        env=env,
        check=True,
    )
    print(f"    -> {out.name} {out.stat().st_size / 1048576:.2f} MB")
    return out


def extract_ffmpeg(goarch: str) -> Path:
    """从缓存 xz 中取出静态 ffmpeg；若不存在则自动下载。"""
    CACHE.mkdir(parents=True, exist_ok=True)
    src = FFMPEG_CACHE[goarch]
    if not src.exists():
        url = FFMPEG_URLS[goarch]
        print(f"  下载 ffmpeg({goarch}): {url}")
        req = urllib.request.Request(url, headers={"User-Agent": "bilidock-build/1.0"})
        with urllib.request.urlopen(req, timeout=300) as resp:
            src.write_bytes(resp.read())
    out = CACHE / f"ffmpeg-{goarch}"
    print(f"  解包 ffmpeg({goarch}) ...")
    with tarfile.open(str(src), "r:xz") as tf:
        for m in tf.getmembers():
            if m.isfile() and m.name.rstrip("/").endswith("/ffmpeg"):
                f = tf.extractfile(m)
                if f is None:
                    continue
                out.write_bytes(f.read())
                out.chmod(0o755)
                print(f"    -> {out.stat().st_size / 1048576:.2f} MB")
                return out
    raise SystemExit(f"{src} 中未找到 ffmpeg 可执行文件")


def _font(size: int) -> ImageFont.FreeTypeFont:
    for p in (
        Path("C:/Windows/Fonts/segoeuib.ttf"),
        Path("C:/Windows/Fonts/arialbd.ttf"),
        Path("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"),
    ):
        if p.exists():
            try:
                return ImageFont.truetype(str(p), size)
            except OSError:
                continue
    return ImageFont.load_default()


def make_icon(size: int, out: Path) -> None:
    scale = 4
    s = size * scale
    img = Image.new("RGBA", (s, s), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    top = (251, 114, 153)
    bottom = (255, 157, 184)
    for y in range(s):
        t = y / max(1, s - 1)
        color = tuple(int(top[i] + (bottom[i] - top[i]) * t) for i in range(3)) + (255,)
        d.line([(0, y), (s, y)], fill=color)
    mask = Image.new("L", (s, s), 0)
    ImageDraw.Draw(mask).rounded_rectangle([0, 0, s - 1, s - 1], radius=int(s * 0.235), fill=255)
    img.putalpha(mask)
    d = ImageDraw.Draw(img)
    font = _font(int(s * 0.62))
    text = "B"
    bbox = d.textbbox((0, 0), text, font=font)
    tx = (s - (bbox[2] - bbox[0])) / 2 - bbox[0]
    ty = (s - (bbox[3] - bbox[1])) / 2 - bbox[1] - int(s * 0.03)
    d.text((tx, ty), text, font=font, fill=(255, 255, 255, 255))
    img.resize((size, size), Image.LANCZOS).save(out)


def build_icons() -> None:
    print("  生成图标 ...")
    images = TEMPLATE / "target" / "ui" / "images"
    images.mkdir(parents=True, exist_ok=True)
    master = MASTER_ICON if MASTER_ICON.exists() else (CACHE / "icon-master.png")
    if master.exists():
        src = Image.open(master).convert("RGBA")
        for size in (16, 24, 32, 48, 64, 72, 96, 128, 256):
            src.resize((size, size), Image.LANCZOS).save(images / f"icon_{size}.png")
        src.resize((72, 72), Image.LANCZOS).save(TEMPLATE / "PACKAGE_ICON.PNG")
        src.resize((256, 256), Image.LANCZOS).save(TEMPLATE / "PACKAGE_ICON_256.PNG")
        src.resize((128, 128), Image.LANCZOS).save(ROOT / "backend" / "web" / "logo.png")
        print(f"    使用主图 {master.relative_to(ROOT)}")
    else:
        for size in (16, 24, 32, 48, 64, 72, 96, 128, 256):
            make_icon(size, images / f"icon_{size}.png")
        make_icon(72, TEMPLATE / "PACKAGE_ICON.PNG")
        make_icon(256, TEMPLATE / "PACKAGE_ICON_256.PNG")
        make_icon(128, ROOT / "backend" / "web" / "logo.png")
        print("    未找到主图，使用程序绘制图标")


def write_cgi_binary(binary: Path) -> None:
    dst = TEMPLATE / "target" / "ui" / "index.cgi"
    dst.write_bytes(binary.read_bytes())
    dst.chmod(0o755)


def build_arch(arch: str, goarch: str, version: str) -> Path:
    print(f"[{arch}]")
    binary = build_backend(goarch, version)
    write_cgi_binary(binary)
    ffmpeg = extract_ffmpeg(goarch)
    out = DIST / f"BiliDock_{version}_{arch}.spk"
    if out.exists():
        out.unlink()
    spk_builder.create_spk(
        output_spk_path=str(out),
        package_id=PKG_ID,
        version=version,
        display_name="BiliDock",
        description="Bilibili video and bangumi downloader with QR login, codec selection, and lossless MP4 muxing",
        description_chs="B 站视频与番剧下载：扫码登录、编码选择、无损混流",
        maintainer="onelxzy",
        maintainer_url="https://github.com/onelxzy/bilidock-spk",
        distributor="onelxzy",
        distributor_url="https://github.com/onelxzy",
        arch=arch,
        binary_content=binary.read_bytes(),
        binary_filename="bilidock",
        dsm_ver="7.0-40851",
        changelog=f"v{version}：正式发布 BiliDock，支持编码选择(AVC/HEVC/AV1)、自由限速、自定义 UA、最高 10 任务/32 线程并发与统一 MP4 无损封装",
        template_dir=str(TEMPLATE),
        binary_dest="bin/bilidock",
        extra_items=[("bin/ffmpeg", str(ffmpeg))],
    )
    print(f"  -> {out.name} {out.stat().st_size / 1048576:.1f} MB")
    return out


def main() -> None:
    version = sys.argv[1] if len(sys.argv) > 1 else "1.0.0"
    os.environ.setdefault("SOURCE_DATE_EPOCH", "1767225600")
    normalize_lf()
    sync_ui_version(version)
    build_icons()
    outputs = []
    for arch, goarch in ARCHES.items():
        outputs.append(build_arch(arch, goarch, version))
    print("\n完成：")
    for o in outputs:
        print(f"  {o}")


if __name__ == "__main__":
    main()
