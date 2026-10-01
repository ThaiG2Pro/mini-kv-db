# /// script
# requires-python = ">=3.10"
# dependencies = ["matplotlib>=3.8"]
# ///
"""Vẽ biểu đồ cho blog từ kết quả bench (bench/...) của các script Linux.

    uv run charts/plot.py                 # tự tìm kết quả mới nhất trong bench/
    uv run charts/plot.py --sample        # dùng dữ liệu mẫu WSL2 trong charts/sample/
    uv run charts/plot.py --out ~/blog/img --phase49 bench/phase4-9/myhost-20261015

Mỗi biểu đồ ra 5 file trong --out (mặc định charts/out/):
    <tên>.light.svg  <tên>.light.png  <tên>.dark.svg  <tên>.dark.png  <tên>.csv
SVG cho nền tảng nhận SVG (sắc nét ở mọi cỡ), PNG 2x cho nền tảng chỉ nhận ảnh raster, CSV cho ai
muốn vẽ lại bằng công cụ của nền tảng (Datawrapper, Flourish, Google Sheets...).

Thiếu dữ liệu cho biểu đồ nào thì bỏ qua biểu đồ đó và in lý do: chạy được với kết quả một phần.
Màu, nét, lưới theo bảng màu đã kiểm tra mù màu (dataviz skill, reference palette).
"""

from __future__ import annotations

import argparse
import csv
import re
import statistics
import sys
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.ticker import FuncFormatter, LogLocator, NullFormatter  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent

# ---------------------------------------------------------------- giao diện

# Reference palette của dataviz skill. Ba slot đầu qua mọi kiểm tra mù màu ở cả hai chế độ
# (validate_palette.js: CVD ΔE 9.2 sáng / 9.4 tối). Slot 3 (aqua) dưới 3:1 trên nền sáng, nên
# đường nào dùng nó cũng có nhãn trực tiếp.
THEMES = {
    "light": dict(surface="#fcfcfb", ink="#0b0b0b", ink2="#52514e", muted="#898781",
                  grid="#e1e0d9", axis="#c3c2b7", s1="#2a78d6", s2="#eb6834", s3="#1baf7a"),
    "dark": dict(surface="#1a1a19", ink="#ffffff", ink2="#c3c2b7", muted="#898781",
                 grid="#2c2c2a", axis="#383835", s1="#3987e5", s2="#d95926", s3="#199e70"),
}
FONT = ["DejaVu Sans", "Liberation Sans", "Arial", "sans-serif"]  # có đủ dấu tiếng Việt
W, H = 8.0, 4.5  # inch; 16:9, vừa khung bài viết của đa số nền tảng


def new_fig(t, title, subtitle, w=W, h=H):
    plt.rcParams.update({"font.family": FONT, "svg.fonttype": "none"})
    fig, ax = plt.subplots(figsize=(w, h), facecolor=t["surface"])
    ax.set_facecolor(t["surface"])
    fig.subplots_adjust(left=0.11, right=0.84, top=0.80, bottom=0.20)
    fig.text(0.03, 0.93, title, color=t["ink"], fontsize=13, fontweight="bold", ha="left")
    fig.text(0.03, 0.875, subtitle, color=t["ink2"], fontsize=9.5, ha="left")
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    for s in ("left", "bottom"):
        ax.spines[s].set_color(t["axis"])
        ax.spines[s].set_linewidth(1)
    ax.tick_params(colors=t["muted"], labelcolor=t["ink2"], labelsize=9, length=0, pad=6)
    ax.grid(True, color=t["grid"], linewidth=0.8, linestyle="-")  # hairline, liền, lùi về sau
    ax.set_axisbelow(True)
    return fig, ax


def axis_labels(ax, t, x, y):
    ax.set_xlabel(x, color=t["ink2"], fontsize=9.5, labelpad=8)
    ax.set_ylabel(y, color=t["ink2"], fontsize=9.5, labelpad=8)


def footnote(fig, t, text):
    fig.text(0.03, 0.03, text, color=t["muted"], fontsize=8, ha="left")


def line(ax, t, xs, ys, color, label):
    # 2px, đầu tròn; marker >= 8px có vòng 2px màu nền để nổi lên khi chồng nét khác.
    ax.plot(xs, ys, color=color, linewidth=2, solid_capstyle="round", solid_joinstyle="round",
            marker="o", markersize=8, markerfacecolor=color, markeredgecolor=t["surface"],
            markeredgewidth=2, label=label, zorder=3)


def end_label(ax, t, x, y, text):
    # Nhãn trực tiếp ở cuối đường: chữ dùng màu chữ, không dùng màu series.
    ax.annotate(text, (x, y), xytext=(8, 0), textcoords="offset points", va="center",
                color=t["ink"], fontsize=9)


def legend(ax, t, loc="upper left"):
    lg = ax.legend(loc=loc, frameon=False, fontsize=9, labelcolor=t["ink2"], handlelength=1.6)
    return lg


def fmt_num(v):
    if v >= 1e6:
        return f"{v/1e6:.1f}M"
    if v >= 1e4:
        return f"{v/1e3:.0f}k"
    if v >= 100:
        return f"{v:.0f}"  # không dấu phân cách nghìn: "1.481" dễ đọc thành một phẩy bốn
    if v >= 10:
        return f"{v:.0f}" if v == int(v) else f"{v:.1f}"
    return f"{v:.2f}"


def save(fig, out, name, theme):
    for ext, kw in (("svg", {}), ("png", {"dpi": 200})):
        fig.savefig(out / f"{name}.{theme}.{ext}", facecolor=fig.get_facecolor(), **kw)
    plt.close(fig)


def write_csv(out, name, header, rows):
    with open(out / f"{name}.csv", "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(header)
        w.writerows(rows)


# ---------------------------------------------------------------- đọc dữ liệu

BENCH_LINE = re.compile(r"^Benchmark(\S+?)(?:-\d+)?\s+(\d+)\s+(.*)$")


def parse_gobench(path: Path) -> dict[str, dict[str, list[float]]]:
    """Đọc output `go test -bench`: tên (bỏ hậu tố -GOMAXPROCS) → đơn vị → các giá trị."""
    res: dict[str, dict[str, list[float]]] = {}
    for ln in path.read_text(errors="replace").splitlines():
        m = BENCH_LINE.match(ln.strip())
        if not m:
            continue
        name, rest = m.group(1), m.group(3).split()
        d = res.setdefault(name, {})
        for v, unit in zip(rest[0::2], rest[1::2]):
            try:
                d.setdefault(unit, []).append(float(v))
            except ValueError:
                pass
    return res


def med(b, name, unit="ns/op"):
    vals = b.get(name, {}).get(unit)
    return statistics.median(vals) if vals else None


def latest(base: Path) -> Path | None:
    """Thư mục kết quả mới nhất: bench/<loại>/<host>-<ngày>/ — xếp theo ngày ở cuối tên."""
    if not base.is_dir():
        return None
    dirs = [d for d in base.iterdir() if d.is_dir()]
    return max(dirs, key=lambda d: (d.name.rsplit("-", 1)[-1], d.stat().st_mtime)) if dirs else None


def gobench(d: Path | None, fname: str):
    if d is None or not (d / fname).is_file():
        return None
    return parse_gobench(d / fname)


# ---------------------------------------------------------------- biểu đồ

class Skip(Exception):
    pass


def need(x, why):
    if x is None:
        raise Skip(why)
    return x


def chart_chain_depth(src, out):
    """Bài 13: chi phí một lần đọc theo độ dài chuỗi version."""
    b = need(gobench(src["phase49"], "p6-get.txt"), "thiếu phase4-9/p6-get.txt")
    depths = sorted({int(m.group(1)) for n in b for m in [re.match(r"GetChainDepth/depth=(\d+)/", n)] if m})
    if not depths:
        raise Skip("p6-get.txt không có GetChainDepth")
    newest = [med(b, f"GetChainDepth/depth={d}/newest") for d in depths]
    oldest = [med(b, f"GetChainDepth/depth={d}/oldest") for d in depths]
    write_csv(out, "chain-depth", ["depth", "newest_ns", "oldest_ns"], zip(depths, newest, oldest))
    for th, t in THEMES.items():
        fig, ax = new_fig(t, "Đọc một khoá có 60 phiên bản tốn bao nhiêu?",
                          "ns mỗi lần Get, theo số phiên bản trong chuỗi · snapshot mới vs snapshot cũ")
        line(ax, t, depths, newest, t["s1"], "snapshot mới (cần bản đầu)")
        line(ax, t, depths, oldest, t["s2"], "snapshot cũ (cần bản cuối)")
        end_label(ax, t, depths[-1], newest[-1], fmt_num(newest[-1]) + " ns")
        axis_labels(ax, t, "số phiên bản trong chuỗi", "ns / lần Get")
        ax.set_ylim(bottom=0)
        legend(ax, t)
        footnote(fig, t, "Nguồn: BenchmarkGetChainDepth (internal/txn) · blog bài 13")
        save(fig, out, "chain-depth", th)


def chart_copy_alloc(src, out):
    """Bài 13: cấp phát + chép vs chỉ chép 897 byte."""
    b = need(gobench(src["phase49"], "p4-copyalloc.txt"), "thiếu phase4-9/p4-copyalloc.txt")
    a, c = med(b, "CopyVsAlloc/alloc+copy"), med(b, "CopyVsAlloc/copy")
    if a is None or c is None:
        raise Skip("p4-copyalloc.txt thiếu nhánh")
    rows = [("chỉ chép", c), ("cấp phát + chép", a)]
    write_csv(out, "copy-vs-alloc", ["cách", "ns"], rows)
    for th, t in THEMES.items():
        title = ("Chép 897 byte gần như miễn phí. Cấp phát thì không." if a / c >= 5
                 else "Cấp phát + chép so với chỉ chép 897 byte")
        fig, ax = new_fig(t, title,
                          f"ns mỗi lần · cấp phát + chép đắt gấp {a/c:.0f} lần chỉ chép", h=3.2)
        ax.grid(axis="y", visible=False)
        ax.barh([r[0] for r in rows], [r[1] for r in rows], height=0.36, color=t["s1"], zorder=3)
        for i, (_, v) in enumerate(rows):
            ax.annotate(f"{fmt_num(v)} ns", (v, i), xytext=(6, 0), textcoords="offset points",
                        va="center", color=t["ink"], fontsize=9.5)
        ax.set_xlim(0, a * 1.18)
        axis_labels(ax, t, "ns / lần", "")
        fig.subplots_adjust(left=0.22, top=0.72, bottom=0.26)
        footnote(fig, t, "Nguồn: BenchmarkCopyVsAlloc (internal/btree) · blog bài 13")
        save(fig, out, "copy-vs-alloc", th)


def chart_commit_batch(src, out):
    """Bài 1 / phase 5: giá một khoá theo số khoá mỗi transaction."""
    b = need(gobench(src["phase49"], "p5-insert.txt"), "thiếu phase4-9/p5-insert.txt")
    ks = [k for k in (1, 10, 100, 1000) if med(b, f"InsertBatch{k}") is not None]
    if len(ks) < 2:
        raise Skip("p5-insert.txt thiếu InsertBatchN")
    ys = [med(b, f"InsertBatch{k}") / 1000 for k in ks]  # µs
    nosync = med(b, "InsertNoSync")
    write_csv(out, "commit-batch", ["khoa_moi_txn", "us_moi_khoa"], zip(ks, ys))
    for th, t in THEMES.items():
        fig, ax = new_fig(t, "Gộp nhiều khoá vào một commit: giá mỗi khoá rơi theo cấp số",
                          f"µs mỗi khoá được chèn · thang log hai trục · gộp 1000 rẻ hơn {ys[0]/ys[-1]:.0f} lần")
        ax.set_xscale("log"), ax.set_yscale("log")
        line(ax, t, ks, ys, t["s1"], "có fsync mỗi commit")
        if nosync:
            ax.axhline(nosync / 1000, color=t["muted"], linewidth=1, zorder=2)
            ax.annotate(f"không fsync: {fmt_num(nosync/1000)} µs", (ks[0], nosync / 1000), xytext=(0, 6),
                        textcoords="offset points", color=t["ink2"], fontsize=9)
        end_label(ax, t, ks[-1], ys[-1], fmt_num(ys[-1]) + " µs")
        ax.set_xticks(ks, [str(k) for k in ks])
        ax.xaxis.set_minor_formatter(NullFormatter())
        ax.yaxis.set_major_formatter(FuncFormatter(lambda v, _: fmt_num(v)))
        axis_labels(ax, t, "số khoá mỗi transaction", "µs / khoá")
        footnote(fig, t, "Nguồn: BenchmarkInsertBatchN, BenchmarkInsertNoSync (internal/db) · blog bài 1")
        save(fig, out, "commit-batch", th)


def cache_sizes(env: Path | None):
    """L2 mỗi nhân và L3 (MiB) từ dòng lscpu trong env.txt."""
    if env is None or not env.is_file():
        return {}
    out = {}
    for ln in env.read_text(errors="replace").splitlines():
        m = re.match(r"\s*(L2|L3) cache:\s+([\d.]+)\s*(KiB|MiB|GiB)(?:\s+\((\d+) instances?\))?", ln)
        if m:
            v = float(m.group(2)) * {"KiB": 1 / 1024, "MiB": 1, "GiB": 1024}[m.group(3)]
            out[m.group(1)] = v / int(m.group(4) or 1) if m.group(1) == "L2" else v
    return out


def chart_getpool(src, out):
    """Phase 4 (nợ P4-6): pool to hơn mà Get chậm hơn, khi pool vượt cache CPU."""
    b = need(gobench(src["phase49"], "p4-getpool.txt"), "thiếu phase4-9/p4-getpool.txt")
    fr = sorted(int(n[7:]) for n in b if re.fullmatch(r"GetPool\d+", n))
    if len(fr) < 3:
        raise Skip("p4-getpool.txt thiếu GetPoolN")
    mb = [f * 4 / 1024 for f in fr]
    ys = [med(b, f"GetPool{f}") for f in fr]
    reads = [med(b, f"GetPool{f}", "reads/op") for f in fr]
    write_csv(out, "getpool", ["frames", "MiB", "ns_op", "reads_op"], zip(fr, mb, ys, reads))
    caches = cache_sizes(src["phase49"] / "env.txt" if src["phase49"] else None) or cache_sizes(
        src["baseline"] / "env.txt" if src["baseline"] else None)
    for th, t in THEMES.items():
        fig, ax = new_fig(t, "Get thay đổi thế nào khi buffer pool lớn dần?",
                          "ns mỗi lần Get theo cỡ pool · đường dọc: cỡ cache CPU của máy đo")
        ax.set_xscale("log", base=2)
        line(ax, t, mb, ys, t["s1"], "ns / Get")
        for name, v in caches.items():
            ax.axvline(v, color=t["muted"], linewidth=1, zorder=2)
            ax.annotate(f"{name}{' / nhân' if name == 'L2' else ''} {v:g} MiB", (v, 1), xycoords=("data", "axes fraction"),
                        xytext=(4, -12), textcoords="offset points", color=t["ink2"], fontsize=8.5)
        ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:g}"))
        ax.set_ylim(bottom=0)
        axis_labels(ax, t, "cỡ buffer pool (MiB, thang log)", "ns / Get")
        footnote(fig, t, "Nguồn: BenchmarkGetPoolN (internal/btree) · nợ P4-6 · cỡ cache đọc từ lscpu trong env.txt")
        save(fig, out, "getpool", th)


# Tỉ số WSL2 (lấy từ diary) — (nhóm, nhãn, giá trị WSL2, file, (tử, đơn vị), (mẫu, đơn vị))
RATIOS = [
    ("P1", "Commit / CommitNoSync", 481, "baseline/p1-pager.txt", ("Commit", "ns/op"), ("CommitNoSync", "ns/op")),
    ("P1", "Commit / mỗi page khi gộp 64", 43, "baseline/p1-pager.txt", ("Commit", "ns/op"), ("CommitBatch64", "ns/page")),
    ("P2", "verifyRef / Verify", 5.5, "baseline/p2-page.txt", ("VerifyRefFullPage", "ns/op"), ("VerifyFullPage", "ns/op")),
    ("P4", "GetPool 2048 / 512 frame", 1.67, "phase4-9/p4-getpool.txt", ("GetPool2048", "ns/op"), ("GetPool512", "ns/op")),
    ("P4", "cấp phát+chép / chỉ chép", 51, "phase4-9/p4-copyalloc.txt", ("CopyVsAlloc/alloc+copy", "ns/op"), ("CopyVsAlloc/copy", "ns/op")),
    ("P5", "1 khoá/txn / 1000 khoá/txn", 261, "phase4-9/p5-insert.txt", ("InsertBatch1", "ns/op"), ("InsertBatch1000", "ns/op")),
    ("P5", "có fsync / không fsync", 453, "phase4-9/p5-insert.txt", ("InsertBatch1", "ns/op"), ("InsertNoSync", "ns/op")),
    ("P6", "commit / abort 0 khoá", 4046, "phase4-9/p6-commit.txt", ("CommitPerLevel/read-uncommitted", "ns/op"), ("Abort/keys=0", "ns/op")),
    ("P6", "abort 1 khoá / 0 khoá", 71, "phase4-9/p6-commit.txt", ("Abort/keys=1", "ns/op"), ("Abort/keys=0", "ns/op")),
    ("P6", "Get serializable / RR", 2.35, "phase4-9/p6-get.txt", ("GetPerLevel/serializable", "ns/op"), ("GetPerLevel/repeatable-read", "ns/op")),
    ("P6", "chuỗi depth 60 / depth 1", 2.4, "phase4-9/p6-get.txt", ("GetChainDepth/depth=60/newest", "ns/op"), ("GetChainDepth/depth=1/newest", "ns/op")),
    ("P6", "lock: 256 / 1 holder", 22.3, "phase4-9/p6-lock.txt", ("AcquireDisjoint/holders=256", "ns/op"), ("AcquireDisjoint/holders=1", "ns/op")),
    ("P7", "so theo kiểu / memcmp", 5.7, "phase4-9/p7-keys.txt", ("CompareVsBytes/tuple", "ns/op"), ("CompareVsBytes/memcmp", "ns/op")),
]


def chart_ratios(src, out, here):
    """Tỉ số nào sống sót khi đổi máy: WSL2 (diary) vs máy vừa đo."""
    rows = []
    for grp, label, wsl, f, (nn, nu), (dn, du) in RATIOS:
        kind, fname = f.split("/")
        b = gobench(src["baseline" if kind == "baseline" else "phase49"], fname)
        if not b:
            continue
        n, d = med(b, nn, nu), med(b, dn, du)
        if n and d:
            rows.append((grp, label, wsl, n / d))
    if len(rows) < 2:
        raise Skip("chưa có đủ file bench của baseline/ hoặc phase4-9/")
    write_csv(out, "ratios", ["nhom", "ti_so", "wsl2", "may_moi"], rows)
    for th, t in THEMES.items():
        h = 1.0 + 0.36 * len(rows)
        fig, ax = new_fig(t, "Tỉ số nào sống sót khi đổi máy?",
                          f"WSL2 (số trong diary) vs {here} · thang log · cùng chỗ là quy luật, lệch xa là đặc tính máy",
                          w=8.8, h=max(h, 4.2))
        fig.subplots_adjust(left=0.36, right=0.94, top=1 - 1.15 / h, bottom=0.75 / h)
        fig.texts[0].set_y(1 - 0.25 / h), fig.texts[1].set_y(1 - 0.5 / h)
        ys = list(range(len(rows)))[::-1]
        for y, (_, _, a, b) in zip(ys, rows):
            ax.plot([a, b], [y, y], color=t["axis"], linewidth=2, zorder=2, solid_capstyle="round")
        ax.scatter([r[2] for r in rows], ys, s=64, color=t["s1"], edgecolors=t["surface"], linewidths=2,
                   zorder=3, label="WSL2 (diary)")
        ax.scatter([r[3] for r in rows], ys, s=64, color=t["s2"], edgecolors=t["surface"], linewidths=2,
                   zorder=4, label=here)
        for y, (_, _, a, b) in zip(ys, rows):
            ax.annotate(f"{b/a:.2g}×", (max(a, b), y), xytext=(10, 0), textcoords="offset points",
                        va="center", color=t["ink2"], fontsize=8.5)
        ax.set_yticks(ys, [f"{g} · {lb}" for g, lb, _, _ in rows])
        ax.set_xscale("log")
        ax.xaxis.set_major_locator(LogLocator(base=10))
        ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:g}×"))
        ax.grid(axis="y", visible=False)
        lo = min(min(r[2], r[3]) for r in rows)
        hi = max(max(r[2], r[3]) for r in rows)
        ax.set_xlim(lo / 1.6, hi * 3)
        ax.legend(loc="lower right", bbox_to_anchor=(1, 1.0), ncol=2, frameon=False, fontsize=9,
                  labelcolor=t["ink2"])
        footnote(fig, t, "Số cạnh mỗi dòng: máy mới / WSL2. Trong khoảng 0.5×–2× thì kết luận của phase đó vẫn đứng.")
        fig.texts[-1].set_y(0.2 / h)
        save(fig, out, "ratios", th)


def chart_p91(src, out):
    """P9-1: tỉ số sau/trước của từng cặp chạy xen kẽ."""
    d = need(src["p91"], "thiếu bench/p91/")
    f = need(d / "pairs.txt" if (d / "pairs.txt").is_file() else None, "thiếu p91/pairs.txt")
    pairs = [l.split() for l in f.read_text().splitlines() if l.strip() and not l.startswith("#")]
    r = sorted(float(p[2]) / float(p[0]) for p in pairs)
    m = statistics.median(r)
    q1, q3 = r[len(r) // 4], r[3 * len(r) // 4]
    write_csv(out, "p91-pairs", ["truoc_ns_hang", "sau_ns_hang", "sau_chia_truoc"],
              [(p[0], p[2], f"{float(p[2])/float(p[0]):.4f}") for p in pairs])
    verdict = "nhanh hơn thật" if q3 < 1 else ("chậm hơn thật" if q1 > 1 else "chưa kết luận được")
    for th, t in THEMES.items():
        fig, ax = new_fig(t, f"Sửa giải mã hàng: seq scan {verdict}",
                          f"thời gian sau / trước của {len(r)} cặp chạy xen kẽ · trung vị {m:.2f} · "
                          f"tứ phân vị [{q1:.2f}, {q3:.2f}]", h=3.4)
        fig.subplots_adjust(bottom=0.30, top=0.72)
        ax.axvspan(q1, q3, color=t["s1"], alpha=0.10, zorder=1)
        ax.axvline(1.0, color=t["muted"], linewidth=1, zorder=2)
        ax.annotate("1.0 = không đổi", (1.0, 0), xycoords=("data", "axes fraction"), xytext=(4, 6),
                    textcoords="offset points", color=t["ink2"], fontsize=8.5)
        jitter = [((i * 7) % 5 - 2) * 0.06 for i in range(len(r))]
        ax.scatter(r, jitter, s=64, color=t["s1"], edgecolors=t["surface"], linewidths=2, zorder=3)
        ax.plot([m, m], [-0.25, 0.25], color=t["ink"], linewidth=2, zorder=4)
        ax.annotate(f"trung vị {m:.2f}", (m, 0.25), xytext=(0, 4), textcoords="offset points",
                    ha="center", color=t["ink"], fontsize=9)
        ax.set_yticks([]), ax.set_ylim(-0.4, 0.45)
        ax.spines["left"].set_visible(False)
        ax.grid(axis="y", visible=False)
        axis_labels(ax, t, "thời gian sau / trước (nhỏ hơn 1 là nhanh hơn)", "")
        footnote(fig, t, "Nguồn: scripts/p91-seqscan.sh (BenchmarkSeqStep) · dải tô: khoảng tứ phân vị · blog bài 13")
        save(fig, out, "p91-pairs", th)


def parse_hashjoin(f: Path):
    """Phép B (chênh theo số hàng probe) và phép E (perf) từ output reallab -work hashjoin."""
    gap, perf, eng, sec = {}, {}, None, None
    for ln in f.read_text(errors="replace").splitlines():
        m = re.match(r"^([A-E])\. (\w+):", ln)
        if m:
            sec, eng = m.group(1), m.group(2)
            continue
        if re.match(r"^[A-E]\. ", ln):
            sec = None
            continue
        if sec == "B":
            m = re.match(r"^\s*(\d)M\s+\S+\s+[\d.]+\s+[\d.]+\s+(-?[\d.]+)\s+(-?[\d.]+)", ln)
            if m:
                gap.setdefault(eng, []).append((int(m.group(1)), float(m.group(2)), float(m.group(3))))
        elif sec == "E":
            m = re.match(r"^(\S+)\s+([\d.]+)\s+([\d.]+)\s+\S+$", ln)
            if m and m.group(1) != "sự":
                perf.setdefault(eng, []).append((m.group(1), float(m.group(2)), float(m.group(3))))
    return gap, perf


def chart_p97(src, out):
    """P9-7: chênh (1 batch − 16 batch) theo số hàng probe; và perf nếu có PMU."""
    d = need(src["p97"], "thiếu bench/p97/")
    f = need(d / "hashjoin.txt" if (d / "hashjoin.txt").is_file() else None, "thiếu p97/hashjoin.txt")
    gap, perf = parse_hashjoin(f)
    if not gap:
        raise Skip("hashjoin.txt không có phép B")
    write_csv(out, "p97-probe-gap", ["engine", "probe_trieu_hang", "chenh_ms", "ns_moi_probe"],
              [(e, k, g, n) for e, rows in gap.items() for k, g, n in rows])
    names = {"pg": "Postgres (malloc mặc định)", "pgm": "Postgres (malloc giữ bộ nhớ)"}
    for th, t in THEMES.items():
        fig, ax = new_fig(t, "Hash join 1 batch chậm thêm bao nhiêu, theo số hàng probe?",
                          "ms chậm thêm so với 16 batch · đường thẳng đi lên = một phí cố định trên mỗi hàng probe")
        for (e, rows), col in zip(gap.items(), (t["s1"], t["s2"], t["s3"])):
            xs, ys = [r[0] for r in rows], [r[1] for r in rows]
            line(ax, t, xs, ys, col, names.get(e, e))
            end_label(ax, t, xs[-1], ys[-1], f"{ys[-1]:.0f} ms")
        ax.set_xticks([1, 2, 3, 4], ["1M", "2M", "3M", "4M"])
        ax.set_ylim(bottom=0)
        axis_labels(ax, t, "số hàng probe", "ms chậm thêm (1 batch − 16 batch)")
        legend(ax, t)
        footnote(fig, t, "Nguồn: reallab -work hashjoin, phép B (trung vị hiệu từng cặp) · nợ P9-7")
        save(fig, out, "p97-probe-gap", th)
    if not perf:
        print("  p97-perf: bỏ qua (hashjoin.txt không có phép E: máy đo không có PMU)")
        return
    for e, rows in perf.items():
        rows = [r for r in rows if r[1] > 0]
        name = f"p97-perf-{e}"
        write_csv(out, name, ["su_kien", "1MB_moi_probe", "256MB_moi_probe", "ti_so"],
                  [(n, a, b, f"{b/a:.3f}") for n, a, b in rows])
        for th, t in THEMES.items():
            fig, ax = new_fig(t, f"Bản 1 batch trượt cache nhiều hơn bao nhiêu? ({e})",
                              "số sự kiện mỗi hàng probe, 256MB (1 batch) chia 1MB (16 batch) · thang log · 1× = như nhau",
                              h=1.4 + 0.42 * len(rows))
            ys = list(range(len(rows)))[::-1]
            ratios = [b / a for _, a, b in rows]
            ax.scatter(ratios, ys, s=64, color=t["s1"], edgecolors=t["surface"], linewidths=2, zorder=3)
            for y, rt, (_, a, b) in zip(ys, ratios, rows):
                ax.annotate(f"{rt:.2f}×  ({fmt_num(a)} → {fmt_num(b)})", (rt, y), xytext=(10, 0),
                            textcoords="offset points", va="center", color=t["ink2"], fontsize=8.5)
            ax.axvline(1.0, color=t["muted"], linewidth=1, zorder=2)
            ax.set_yticks(ys, [r[0] for r in rows])
            ax.set_xscale("log")
            ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:g}×"))
            ax.set_xlim(min(ratios + [1]) / 1.5, max(ratios + [1]) * 4)
            ax.grid(axis="y", visible=False)
            fig.subplots_adjust(left=0.34)
            footnote(fig, t, "Nguồn: reallab -work hashjoin, phép E (perf stat trên backend) · nợ P9-7")
            save(fig, out, name, th)


def chart_group_commit(src, out):
    """Bài 1: bao nhiêu commit dùng chung một fsync, theo số client."""
    d = need(src["phase49"], "thiếu bench/phase4-9/")
    f = need(d / "p9-commit.txt" if (d / "p9-commit.txt").is_file() else None, "thiếu phase4-9/p9-commit.txt")
    rows = []
    for ln in f.read_text().splitlines():
        m = re.search(r"clients=(\d+)\s+commits=(\d+)\s+wal_fsyncs=(\d+)", ln)
        if m and int(m.group(3)) > 0:
            rows.append((int(m.group(1)), int(m.group(2)) / int(m.group(3))))
    if len(rows) < 2:
        raise Skip("p9-commit.txt chưa có dòng clients=… wal_fsyncs=…")
    write_csv(out, "group-commit", ["clients", "commit_moi_fsync"], rows)
    for th, t in THEMES.items():
        fig, ax = new_fig(t, "Nhiều client cùng commit thì chia nhau một lần fsync",
                          "số commit dùng chung một fsync của WAL, theo số client · Postgres, pgbench")
        xs, ys = [r[0] for r in rows], [r[1] for r in rows]
        ax.set_xscale("log", base=2)
        line(ax, t, xs, ys, t["s1"], "commit / fsync")
        end_label(ax, t, xs[-1], ys[-1], f"{ys[-1]:.1f}")
        ax.set_xticks(xs, [str(x) for x in xs])
        ax.xaxis.set_minor_formatter(NullFormatter())
        ax.set_ylim(bottom=0)
        axis_labels(ax, t, "số client đồng thời", "commit / fsync")
        footnote(fig, t, "Nguồn: blog/lab/01-group-commit.sh (pg_stat_wal.wal_sync) · blog bài 1")
        save(fig, out, "group-commit", th)


# ---------------------------------------------------------------- main

def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--sample", action="store_true", help="dùng dữ liệu mẫu WSL2 trong charts/sample/")
    ap.add_argument("--baseline", type=Path, help="thư mục của linux-baseline.sh (mặc định: mới nhất)")
    ap.add_argument("--phase49", type=Path, help="thư mục của linux-phase4-9.sh (mặc định: mới nhất)")
    ap.add_argument("--p91", type=Path, help="thư mục của p91-seqscan.sh (mặc định: mới nhất)")
    ap.add_argument("--p97", type=Path, help="thư mục của p97-hashjoin.sh (mặc định: mới nhất)")
    ap.add_argument("--out", type=Path, default=ROOT / "charts" / "out")
    ap.add_argument("--label", default=None, help='tên máy mới trong biểu đồ tỉ số (mặc định: "Linux thuần")')
    a = ap.parse_args()

    if a.sample:
        s = ROOT / "charts" / "sample"
        src = {"baseline": s / "baseline", "phase49": s / "phase4-9", "p91": s / "p91", "p97": s / "p97"}
        here = a.label or "WSL2 chạy lại (mẫu)"
    else:
        src = {k: getattr(a, k) or latest(ROOT / "bench" / sub) for k, sub in
               (("baseline", "baseline"), ("phase49", "phase4-9"), ("p91", "p91"), ("p97", "p97"))}
        here = a.label or "Linux thuần"
    a.out.mkdir(parents=True, exist_ok=True)
    print("nguồn:", *(f"  {k}: {v}" for k, v in src.items()), sep="\n")

    charts = [
        ("ratios", lambda: chart_ratios(src, a.out, here)),
        ("chain-depth", lambda: chart_chain_depth(src, a.out)),
        ("copy-vs-alloc", lambda: chart_copy_alloc(src, a.out)),
        ("commit-batch", lambda: chart_commit_batch(src, a.out)),
        ("getpool", lambda: chart_getpool(src, a.out)),
        ("group-commit", lambda: chart_group_commit(src, a.out)),
        ("p91-pairs", lambda: chart_p91(src, a.out)),
        ("p97", lambda: chart_p97(src, a.out)),
    ]
    made = 0
    for name, fn in charts:
        try:
            fn()
            made += 1
            print(f"  {name}: ok")
        except Skip as e:
            print(f"  {name}: bỏ qua ({e})")
    print(f"\n{made}/{len(charts)} biểu đồ → {a.out}")
    return 0 if made else 1


if __name__ == "__main__":
    sys.exit(main())
