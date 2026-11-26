"""2026-11-21: policy-document upload text extraction (app/extract.py)."""
import io

import pytest

from app.extract import ExtractError, extract_text


def _pdf(text: str) -> bytes:
    """A one-page PDF with a real text layer (hand-built, no generator dependency)."""
    stream = f"BT /F1 12 Tf 72 720 Td ({text}) Tj ET".encode()
    objs = [b"<< /Type /Catalog /Pages 2 0 R >>", b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
            b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
            b"<< /Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream",
            b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"]
    out, offs = io.BytesIO(), []
    out.write(b"%PDF-1.4\n")
    for i, o in enumerate(objs, 1):
        offs.append(out.tell()); out.write(b"%d 0 obj\n" % i + o + b"\nendobj\n")
    x = out.tell()
    out.write(b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1))
    for o in offs:
        out.write(b"%010d 00000 n \n" % o)
    out.write(b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, x))
    return out.getvalue()


def test_pdf_text_layer():
    text, pages = extract_text("Contributor Policy.PDF", _pdf("Early access: 25 percent after four months of unemployment."))
    assert pages == 1 and "[Page 1]" in text and "25 percent after four months" in text


def test_scanned_pdf_points_to_ocr():
    with pytest.raises(ExtractError, match="Upload scan"):
        extract_text("scan.pdf", _pdf(""))


def test_docx_paragraphs_and_tables():
    import docx
    d = docx.Document(); d.add_paragraph("Section 1: Contributions are 8% from the employee and 10% from the employer.")
    t = d.add_table(rows=1, cols=2); t.cell(0, 0).text = "Fund II"; t.cell(0, 1).text = "Default for contributors under 50"
    buf = io.BytesIO(); d.save(buf)
    text, pages = extract_text("policy.docx", buf.getvalue())
    assert pages == 0 and "8% from the employee" in text and "Fund II | Default for contributors under 50" in text


def test_txt_and_rejections():
    assert extract_text("faq.md", "Q: When can I retire?\nA: From age 50.".encode())[0].startswith("Q: When")
    for name, data, msg in [("x.exe", b"MZ" * 20, "Upload a PDF"), ("e.txt", b"", "empty"), ("bad.pdf", b"not a pdf at all" * 3, "not a readable PDF"),
                            ("bad.docx", b"zipzipzip" * 5, "not a readable Word")]:
        with pytest.raises(ExtractError, match=msg):
            extract_text(name, data)
