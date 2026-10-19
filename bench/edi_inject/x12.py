"""Writes X12 004010 interchanges for the corpus.

Input: a list of segments (each a list of element strings) and the envelope values.
Output: the raw bytes of one interchange with one group and one set (ISA exactly 106 bytes,
SE, GE and IEA counts filled in). The three `segments_*` functions turn a content dict
(made by content.py) into the segments of an 850, an 810 or a 997, using only the
segments and lengths that the gateway grammar accepts.

The separators are the first of three fixed sets that has none of its characters in any
element value. If none is free, the generator fails with GenerationError: a text is never
escaped (004010 has no release character).
"""

GATEWAY = "ZZTESTGATEWAY"
ISA09_DATE = "261019"
ISA10_TIME = "1200"
GS_DATE = "20261019"
GS_TIME = "1200"
GROUP_CODE = {"850": "PO", "810": "IN", "997": "FA"}

# (element separator, component separator, segment terminator)
SEPARATOR_SETS = (("*", ">", "~"), ("|", "^", "\\"), ("!", "@", "`"))

FILLER_NAME = "Order desk"
FILLER_PHONE = "555-0100"
FILLER_FAX = "555-0101"


class GenerationError(Exception):
    """The generator cannot build the document as planned."""


def pick_separators(segments):
    """The first separator set whose characters occur in no element of `segments`."""
    text = "".join(e for seg in segments for e in seg)
    for chosen in SEPARATOR_SETS:
        if not any(c in text for c in chosen):
            return chosen
    raise GenerationError("no separator set is free of the characters of the document values")


def build_interchange(sender, number, st01, segments):
    """Raw bytes of one interchange. `sender` is the ISA06 text (for example ZZTESTBUY01)."""
    es, cs, term = pick_separators(segments)
    isa_fields = [
        "00", " " * 10, "00", " " * 10, "ZZ", sender.ljust(15), "ZZ", GATEWAY.ljust(15),
        ISA09_DATE, ISA10_TIME, "U", "00401", f"{number:09d}", "0", "T",
    ]
    isa = "ISA" + "".join(es + f for f in isa_fields) + es + cs + term
    if len(isa.encode("ascii")) != 106:
        raise GenerationError("ISA is not 106 bytes long")

    def render(seg):
        elements = list(seg)
        while len(elements) > 1 and elements[-1] == "":
            elements.pop()
        return es.join(elements) + term

    inner = [["ST", st01, "0001"]] + segments
    inner.append(["SE", str(len(inner) + 1), "0001"])
    gs = ["GS", GROUP_CODE[st01], sender, GATEWAY, GS_DATE, GS_TIME, str(number), "X", "004010"]
    out = [isa] + [render(s) for s in [gs] + inner]
    out += [render(["GE", "1", str(number)]), render(["IEA", "1", f"{number:09d}"])]
    return "".join(out).encode("utf-8")


def _per(c, texts):
    """The PER segment: filler up to the highest free-text position that is used."""
    positions = [p for p in ("PER02", "PER04", "PER06", "PER08") if p in texts]
    if not positions:
        return None
    seg = ["PER", "IC", texts.get("PER02", FILLER_NAME)]
    if positions[-1] != "PER02":
        seg += ["EM", texts.get("PER04", c["contact_email"])]
    if positions[-1] in ("PER06", "PER08"):
        seg += ["TE", texts.get("PER06", FILLER_PHONE)]
    if positions[-1] == "PER08":
        seg += ["FX", texts["PER08"]]
    return seg


def _n2(texts):
    if "N2-01" not in texts and "N2-02" not in texts:
        return None
    return ["N2", texts.get("N2-01", "Receiving"), texts.get("N2-02", "")]


def _name(c, party, key):
    """Name of an N1 party: the N1-02 text when the document puts its text there."""
    if "N1-02" in c["texts"] and c["n1_target"] == party:
        return c["texts"]["N1-02"]
    return c[key]


def segments_850(c):
    """Segments of a purchase order. `c` keys: see content.compose_850."""
    t = c["texts"]
    segs = [["BEG", "00", "SA", c["business_ref"], "", c["date"]]]
    if c["currency"]:
        segs.append(["CUR", "BY", "USD"])
    segs.append(["REF", "CR", c["ref02"], t.get("REF03", "")])
    per = _per(c, t)
    if per:
        segs.append(per)
    if "MSG01" in t:
        segs += [["N9", "ZZ", "NOTE"], ["MSG", t["MSG01"]]]
    segs.append(["N1", "BY", _name(c, "BY", "buyer_name"), "92", c["buyer_code"]])
    n2 = _n2(t)
    if n2:
        segs.append(n2)
    segs.append(["N1", "ST", _name(c, "ST", "ship_name"), "92", c["ship_code"]])
    if "MTX02" in t or "MTX03" in t:
        segs += [["ADV", "ZZ", "AB"], ["MTX", "GEN", t.get("MTX02", ""), t.get("MTX03", "")]]
    for i, line in enumerate(c["lines"]):
        segs.append(["PO1", str(i + 1), str(line["qty"]), "EA", line["price"], "", "VN", line["sku"]])
        if i == c["pid_line"] and "PID05" in t:
            segs.append(["PID", "F", "", "", "", t["PID05"]])
    segs.append(["CTT", str(len(c["lines"]))])
    return segs


def segments_810(c):
    """Segments of a supplier invoice. `c` keys: see content.compose_810."""
    t = c["texts"]
    segs = [["BIG", c["date"], c["business_ref"], c["po_date"], c["order_number"]]]
    if "NTE02" in t:
        segs.append(["NTE", "GEN", t["NTE02"]])
    if c["currency"]:
        segs.append(["CUR", "SE", "USD"])
    segs.append(["REF", "IV", c["ref02"], t.get("REF03", "")])
    per = _per(c, t)
    if per:
        segs.append(per)
    segs.append(["N1", "SU", _name(c, "SU", "supplier_name"), "92", c["supplier_code"]])
    segs.append(["N1", "BY", _name(c, "BY", "buyer_name"), "92", c["buyer_code"]])
    n2 = _n2(t)
    if n2:
        segs.append(n2)
    segs.append(["N1", "ST", _name(c, "ST", "ship_name"), "92", c["ship_code"]])
    for i, line in enumerate(c["lines"]):
        segs.append(["IT1", str(i + 1), str(line["qty"]), "EA", line["price"], "", "VN", line["sku"]])
    segs.append(["TDS", c["total_cents"]])
    segs.append(["CTT", str(len(c["lines"]))])
    return segs


def segments_997(c):
    """Segments of an acknowledgement. The control numbers are stubs (see the README)."""
    ak5 = c["ak5"]
    sets_accepted = "0" if ak5 == "R" else "1"
    group = "PR" if c["ack_target"] == "855" else "SH"
    ak5_seg = ["AK5", ak5] + (["5"] if ak5 != "A" else [])
    return [
        ["AK1", group, "1"], ["AK2", c["ack_target"], "0001"], ak5_seg,
        ["AK9", ak5, "1", "1", sets_accepted],
    ]
