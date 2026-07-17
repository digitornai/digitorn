#!/usr/bin/env python3
"""Run-aware find/replace for word/document.xml — the surgical edit primitive.

WHY THIS EXISTS
    Word splits a visible sentence across many <w:r> runs (revision ids,
    spell-check state, a bold word, a styled date). So a phrase you can SEE
    often does not exist as a contiguous string in the XML: a plain sed finds
    nothing, changes nothing, and reports success. Measured on a real
    Word-authored document: 14% of 6-word targets are unreachable that way, and
    merge_runs.py cannot help — it only coalesces runs of IDENTICAL formatting
    (merging across formatting would destroy it).

WHAT IT DOES
    Works on each paragraph's CONCATENATED text (so the target is always
    findable), maps the match back to the exact <w:t> elements and offsets that
    carry it, and rewrites only those — every run keeps its own formatting. The
    replacement adopts the first matched run's formatting, which is what Word
    does when you type over a selection.

    Fails LOUDLY (exit 1) when the target is absent: a silent no-op on a
    contract is worse than an error.

USAGE
    python3 replace_text.py unpacked/ "ancien" "nouveau"          # 1st occurrence
    python3 replace_text.py unpacked/ "ancien" "nouveau" --all
    python3 replace_text.py unpacked/ "ancien" "nouveau" --expect 3
    python3 replace_text.py unpacked/ "ancien" --find             # dry-run, locate only
    python3 replace_text.py doc.docx  "ancien" "nouveau" -o out.docx

    A directory argument edits word/document.xml in place (the unzip → edit →
    zip flow). A .docx argument unzips, edits and rezips for you.
"""
from __future__ import annotations

import argparse
import re
import shutil
import sys
import tempfile
import zipfile
from pathlib import Path

import lxml.etree

W = "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}"
XML_SPACE = "{http://www.w3.org/XML/1998/namespace}space"


def _texts(paragraph) -> list:
    """The <w:t> elements of a paragraph, in document order.

    Deleted text (<w:delText>) is skipped: it is not visible, so it must never
    be matched. Text inside a nested table cell belongs to that cell's own
    <w:p>, so iterating paragraphs already covers tables.
    """
    return [t for t in paragraph.iter(f"{W}t")]


def _paragraph_text(tt: list) -> str:
    return "".join(t.text or "" for t in tt)


def _spans(tt: list):
    """[(w:t element, start, end)] — each element's slice of the concatenated text."""
    out, pos = [], 0
    for t in tt:
        n = len(t.text or "")
        out.append((t, pos, pos + n))
        pos += n
    return out


def _set(t, value: str) -> None:
    """Write text back, keeping leading/trailing spaces significant."""
    t.text = value
    if value != value.strip():
        t.set(XML_SPACE, "preserve")


def _apply(tt: list, start: int, end: int, new: str) -> None:
    """Replace [start,end) of the concatenated text with `new`.

    The first overlapped run receives the whole replacement (its formatting
    wins); the remaining overlapped runs lose only their matched slice. Runs
    outside the match are never touched.
    """
    first = True
    for t, a, b in _spans(tt):
        if b <= start or a >= end:
            continue  # untouched run
        cur = t.text or ""
        head = cur[: max(0, start - a)]
        tail = cur[max(0, min(len(cur), end - a)) :]
        if first:
            _set(t, head + new + tail)
            first = False
        else:
            _set(t, head + tail)


def _iter_paragraphs(root):
    return root.iter(f"{W}p")


def replace_in_tree(root, target: str, new: str | None, replace_all: bool):
    """Returns [(paragraph_text, count)] of the paragraphs that matched."""
    hits = []
    done = 0
    for p in _iter_paragraphs(root):
        tt = _texts(p)
        if not tt:
            continue
        while True:
            text = _paragraph_text(tt)
            idx = text.find(target)
            if idx < 0:
                break
            hits.append(text)
            if new is None:  # --find : locate only
                break
            _apply(tt, idx, idx + len(target), new)
            done += 1
            if not replace_all:
                return hits, done
            # continue scanning the same paragraph for further occurrences
        if new is None and hits and not replace_all and hits:
            pass
    return hits, done


def main() -> int:
    ap = argparse.ArgumentParser(description="Run-aware find/replace in a .docx")
    ap.add_argument("target_path", help="unpacked/ directory or a .docx file")
    ap.add_argument("find", help="visible text to look for")
    ap.add_argument("replace", nargs="?", help="replacement text (omit with --find)")
    ap.add_argument("-o", "--output", help="output .docx (when input is a .docx)")
    ap.add_argument("--all", action="store_true", help="replace every occurrence")
    ap.add_argument("--find", dest="find_only", action="store_true", help="locate only, change nothing")
    ap.add_argument("--expect", type=int, help="fail unless exactly N replacements happen")
    args = ap.parse_args()

    if not args.find_only and args.replace is None:
        print("error: give a replacement, or use --find to locate only", file=sys.stderr)
        return 2

    src = Path(args.target_path)
    tmp = None
    if src.is_dir():
        doc_xml = src / "word" / "document.xml"
    elif src.suffix.lower() in (".docx", ".dotx"):
        tmp = Path(tempfile.mkdtemp(prefix="rt-"))
        with zipfile.ZipFile(src) as z:
            z.extractall(tmp)
        doc_xml = tmp / "word" / "document.xml"
    else:
        print(f"error: {src} is neither a directory nor a .docx", file=sys.stderr)
        return 2
    if not doc_xml.exists():
        print(f"error: {doc_xml} not found", file=sys.stderr)
        return 2

    tree = lxml.etree.parse(str(doc_xml))
    root = tree.getroot()
    hits, done = replace_in_tree(root, args.find, None if args.find_only else args.replace, args.all)

    if not hits:
        print(f'NOT FOUND: "{args.find}" appears nowhere in the visible text.', file=sys.stderr)
        print("  The text is matched across runs, so fragmentation is NOT the cause —", file=sys.stderr)
        print("  check the exact wording (quotes, non-breaking spaces, casing).", file=sys.stderr)
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)
        return 1

    if args.find_only:
        print(f'FOUND {len(hits)} paragraph(s) containing "{args.find}":')
        for h in hits[:10]:
            i = h.find(args.find)
            print(f"  … {h[max(0, i-40):i+len(args.find)+40].strip()} …")
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)
        return 0

    if args.expect is not None and done != args.expect:
        print(f"error: expected {args.expect} replacement(s), made {done} — nothing written.", file=sys.stderr)
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)
        return 1

    tree.write(str(doc_xml), xml_declaration=True, encoding="UTF-8", standalone=True)

    if tmp:
        out = Path(args.output) if args.output else src
        if out.exists() and out.samefile(src) and not args.output:
            print("error: refusing to overwrite the source; pass -o OUT.docx", file=sys.stderr)
            shutil.rmtree(tmp, ignore_errors=True)
            return 2
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
            for f in sorted(tmp.rglob("*")):
                if f.is_file():
                    z.write(f, f.relative_to(tmp).as_posix())
        shutil.rmtree(tmp, ignore_errors=True)
        print(f"Replaced {done} occurrence(s); wrote {out}")
    else:
        print(f"Replaced {done} occurrence(s) in {doc_xml}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
