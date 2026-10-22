"""Draws the design figures as SVG: python3 docs/figures/figures.py"""

import pathlib
from html import escape

OUT = pathlib.Path(__file__).parent
FONT = "-apple-system, 'Segoe UI', Helvetica, Arial, sans-serif"

C = {
    "blue": "#1a73e8",
    "blue_bg": "#e8f0fe",
    "green": "#1e8e3e",
    "green_cell": "#34a853",
    "green_bg": "#e6f4ea",
    "amber": "#f29900",
    "amber_cell": "#f9ab00",
    "amber_bg": "#fef7e0",
    "red": "#d93025",
    "red_bg": "#fce8e6",
    "grey": "#5f6368",
    "grey_cell": "#dadce0",
    "grey_bg": "#f1f3f4",
    "purple": "#8430ce",
    "purple_bg": "#f3e8fd",
    "ink": "#202124",
    "muted": "#5f6368",
    "rule": "#dadce0",
    "lane": "#c4c7c5",
}
STATE = {
    "active": ("green", "green_bg", "green_cell"),
    "paused": ("amber", "amber_bg", "amber_cell"),
    "joining": ("amber", "amber_bg", "amber_cell"),
    "failed": ("red", "red_bg", "red"),
    "starting": ("grey", "grey_bg", "grey_cell"),
}


def width(text, size, bold=False):
    return len(text) * size * (0.6 if bold else 0.55)


class Svg:
    def __init__(self, w, h, title=None, subtitle=None):
        self.w, self.h, self.parts = w, h, []
        if title:
            self.text(32, 42, title, 20, bold=True)
        if subtitle:
            self.text(32, 66, subtitle, 13, color=C["muted"])

    def text(self, x, y, s, size=13, color=None, bold=False, anchor="start", italic=False):
        weight = ' font-weight="700"' if bold else ""
        style = ' font-style="italic"' if italic else ""
        self.parts.append(
            f'<text x="{x:.1f}" y="{y:.1f}" font-size="{size}" fill="{color or C["ink"]}" text-anchor="{anchor}"'
            f'{weight}{style} xml:space="preserve">{escape(s)}</text>'
        )

    def rect(self, x, y, w, h, stroke, fill, r=6, sw=2, dash=None):
        d = f' stroke-dasharray="{dash}"' if dash else ""
        self.parts.append(
            f'<rect x="{x:.1f}" y="{y:.1f}" width="{w:.1f}" height="{h:.1f}" rx="{r}" fill="{fill}" '
            f'stroke="{stroke}" stroke-width="{sw}"{d}/>'
        )

    def line(self, x1, y1, x2, y2, color, sw=1.5, dash=None):
        d = f' stroke-dasharray="{dash}"' if dash else ""
        self.parts.append(
            f'<line x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}" stroke="{color}" stroke-width="{sw}"{d}/>'
        )

    def box(self, x, y, w, h, title, lines=(), tone="grey", fill=None, dash=None, center=False, size=14):
        stroke = C[tone]
        self.rect(x, y, w, h, stroke, fill or C[tone + "_bg"], dash=dash)
        block = 20 + 18 * len(lines)
        ty = y + (h - block) / 2 + 15 if center else y + 24
        tx, anchor = (x + w / 2, "middle") if center else (x + 14, "start")
        self.text(tx, ty, title, size, color=stroke if center else C["ink"], bold=True, anchor=anchor)
        for i, line in enumerate(lines):
            self.text(tx, ty + 20 + 18 * i, line, 12.5, color=C["muted"], anchor=anchor)

    def arrow(
        self, points, color, label=None, at=None, dash=None, sw=1.8, label_color=None, anchor="middle", head=True
    ):
        d = "M " + " L ".join(f"{x:.1f} {y:.1f}" for x, y in points)
        da = f' stroke-dasharray="{dash}"' if dash else ""
        self.parts.append(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="{sw}"{da}/>')
        if head:
            (x1, y1), (x2, y2) = points[-2], points[-1]
            self.head(x2, y2, x1, y1, color)
        if label:
            lx, ly = at
            for i, part in enumerate(label if isinstance(label, (list, tuple)) else [label]):
                self.text(lx, ly + 16 * i, part, 12, color=label_color or color, anchor=anchor)

    def head(self, x, y, fx, fy, color):
        import math

        a = math.atan2(y - fy, x - fx)
        p = [
            (x, y),
            (x - 10 * math.cos(a - 0.4), y - 10 * math.sin(a - 0.4)),
            (x - 10 * math.cos(a + 0.4), y - 10 * math.sin(a + 0.4)),
        ]
        self.parts.append(f'<polygon points="{" ".join(f"{px:.1f},{py:.1f}" for px, py in p)}" fill="{color}"/>')

    def legend(self, y, items, x=32, step=None):
        cx = x
        for kind, color, label in items:
            if kind in ("line", "dash"):
                self.line(cx, y - 4, cx + 30, y - 4, C[color], 2, "6 4" if kind == "dash" else None)
                cx += 38
            else:
                tone, bg, _ = STATE.get(kind, (color, color + "_bg", None))
                self.rect(cx, y - 13, 26, 16, C[tone], C[bg], r=3, dash="4 3" if kind == "starting" else None)
                cx += 34
            self.text(cx, y, label, 12.5, color=C["muted"])
            cx += step or width(label, 12.5) + 30

    def save(self, name):
        body = "\n".join(self.parts)
        svg = (
            f'<svg xmlns="http://www.w3.org/2000/svg" width="{self.w}" height="{self.h}" '
            f'viewBox="0 0 {self.w} {self.h}" '
            f'font-family="{FONT}">\n<rect width="100%" height="100%" rx="8" fill="#ffffff"/>\n{body}\n</svg>\n'
        )
        (OUT / name).write_text(svg)


def sequence(name, title, lanes, phases, subtitle=None, lane_w=190, legend=None):
    left = 32
    xs = {key: left + 150 + lane_w * i + lane_w / 2 for i, (key, *_rest) in enumerate(lanes)}
    w = int(left + 150 + lane_w * len(lanes) + 24)
    rows = sum(1 + sum(1.3 + 0.45 * (len(it[3]) - 1) if it[0] == "note" else 1 for it in items) for _, items in phases)
    top = 140 if subtitle else 116
    h = int(top + 30 + rows * 40 + 70)
    s = Svg(w, h, title, subtitle)
    hy = top - 46
    for key, label, tone in lanes:
        s.rect(
            xs[key] - lane_w / 2 + 14, hy, lane_w - 28, 44, C[tone], C["grey_bg"] if tone == "grey" else C[tone + "_bg"]
        )
        s.text(xs[key], hy + 27, label, 13.5, color=C[tone], bold=True, anchor="middle")
    s.parts.append("<g>")
    lane_parts = len(s.parts)
    y = top + 18
    for phase_title, items in phases:
        s.line(left - 8, y - 8, w - 16, y - 8, C["rule"], 1.2)
        s.text(left, y + 12, phase_title, 14, color=C["blue"], bold=True)
        y += 40
        for item in items:
            if item[0] == "note":
                _, a, b, lines, tone = item
                x1, x2 = xs[a] - 80, xs[b] + 80
                s.rect(x1, y - 14, x2 - x1, 22 + 18 * (len(lines) - 1) + 12, C[tone], C[tone + "_bg"], r=5, sw=1.5)
                for i, line in enumerate(lines):
                    s.text(x1 + 12, y + 6 + 18 * i, line, 12.5)
                y += 52 + 18 * (len(lines) - 1)
                continue
            _, a, b, label, reply = item[:5]
            tone = item[5] if len(item) > 5 else ("blue" if a == "c" else "green")
            x1, x2 = xs[a], xs[b]
            pad = 3 if x2 > x1 else -3
            s.arrow([(x1, y), (x2 - pad, y)], C[tone], label, ((x1 + x2) / 2, y - 7), dash="7 5" if reply else None)
            y += 40
    for key, *_ in lanes:
        s.parts.insert(
            lane_parts,
            f'<line x1="{xs[key]:.1f}" y1="{hy + 44}" x2="{xs[key]:.1f}" y2="{y - 10:.1f}" '
            f'stroke="{C["lane"]}" stroke-width="1.3" stroke-dasharray="5 5"/>',
        )
    s.parts.append("</g>")
    s.line(left - 8, y, w - 16, y, C["rule"], 1.2)
    s.legend(
        y + 34,
        legend
        or [
            ("line", "blue", "call"),
            ("dash", "blue", "reply"),
            ("amber", "amber", "decision"),
            ("green", "green", "engine"),
        ],
    )
    s.h = int(y + 56)
    s.save(name)


def pod(s, x, y, name, sub, cells, status, state, w=None, crossed=()):
    cw, gap = 54, 6
    w = w or max(14 * 2 + len(cells) * (cw + gap) - gap, 170)
    tone, bg, _ = STATE[state]
    dash = "4 4" if state == "starting" else None
    s.rect(x, y, w, 118, C[tone], C[bg] if state != "starting" else "#ffffff", dash=dash)
    s.text(x + 12, y + 22, name, 14, bold=True)
    s.text(x + 12, y + 40, sub, 12, color=C["muted"])
    for i, (label, cstate) in enumerate(cells):
        cx = x + 14 + i * (cw + gap)
        fill = C[STATE[cstate][2]]
        s.rect(cx, y + 52, cw, 28, fill, fill, r=3, sw=1)
        s.text(
            cx + cw / 2,
            y + 71,
            label,
            12,
            color="#ffffff" if cstate != "starting" else C["muted"],
            bold=True,
            anchor="middle",
        )
        if label in crossed:
            s.line(cx + 6, y + 56, cx + cw - 6, y + 76, "#ffffff", 2.5)
            s.line(cx + cw - 6, y + 56, cx + 6, y + 76, "#ffffff", 2.5)
    s.rect(x + 14, y + 88, w - 28, 20, C[tone], C[tone] if state != "starting" else C["grey_cell"], r=3, sw=1)
    s.text(
        x + w / 2, y + 102, status, 12, color="#ffffff" if state != "starting" else C["ink"], bold=True, anchor="middle"
    )
    return w


def architecture():
    s = Svg(1200, 820, "IRO at a glance", "one manager per engine group · the controller only talks to its plugins")
    s.rect(30, 92, 790, 620, C["blue"], "#fbfcff", r=10)
    s.text(48, 118, "IRO manager  (leader-elected)", 14, color=C["blue"], bold=True)
    s.box(60, 330, 210, 100, "Controller", ["one step per poll", "saves, executes, checks"], tone="blue")
    s.box(540, 140, 260, 66, "Journal", ["versioned record, Load / Save"], tone="grey")
    s.box(540, 260, 260, 78, "Workload", ["one group of Pods", "Pod identity, Reset, Release"], tone="grey")
    s.rect(540, 380, 260, 170, C["green"], C["green_bg"])
    s.text(554, 404, "Engine", 14, bold=True)
    s.text(554, 424, "one plugin per inference engine", 12.5, color=C["muted"])
    s.box(554, 442, 112, 90, "Policy", ["pure: decides", "no I/O"], tone="green", fill="#ffffff")
    s.box(676, 442, 112, 90, "Driver", ["reads, applies", "over HTTP"], tone="green", fill="#ffffff")
    s.box(540, 620, 260, 60, "Telemetry", ["metrics, Events, logs"], tone="grey")
    s.arrow([(165, 330), (165, 173), (540, 173)], C["blue"], "Load, Save", (350, 165))
    s.arrow(
        [(270, 360), (470, 360), (470, 299), (540, 299)],
        C["blue"],
        ["Discover, Reset,", "ResetAll, Release"],
        (284, 336),
        anchor="start",
    )
    s.arrow(
        [(270, 400), (470, 400), (470, 465), (540, 465)],
        C["blue"],
        ["Observe, Apply", "Decide"],
        (284, 422),
        anchor="start",
    )
    s.arrow([(165, 430), (165, 650), (540, 650)], C["blue"], "what happened", (350, 642))
    s.box(890, 260, 280, 78, "Kubernetes + workload controller", ["recreates deleted Pods"], tone="grey", center=True)
    s.box(890, 420, 280, 80, "Engine runtime", ["members and their recovery API"], tone="green", center=True)
    s.box(890, 530, 280, 56, "Traffic router (EPP)", [], tone="purple", center=True)
    s.box(890, 620, 280, 60, "Prometheus, Events, logs", [], tone="grey", center=True)
    s.arrow([(800, 299), (890, 299)], C["grey"], "Pods", (845, 291))
    s.arrow([(788, 465), (890, 465)], C["green"], "HTTP", (840, 457))
    s.arrow([(800, 650), (890, 650)], C["grey"], "export", (845, 642))
    s.arrow([(1030, 530), (1030, 500)], C["purple"], "routes on native health", (1042, 520), anchor="start")
    s.legend(
        760,
        [
            ("line", "blue", "controller call"),
            ("line", "green", "engine HTTP"),
            ("line", "grey", "Kubernetes / export"),
            ("line", "purple", "traffic"),
        ],
    )
    s.save("architecture.svg")


def step():
    sequence(
        "controller-step.svg",
        "One controller step",
        [
            ("c", "Controller", "blue"),
            ("w", "Workload", "grey"),
            ("d", "Engine driver", "green"),
            ("p", "Engine policy", "green"),
            ("j", "Journal", "grey"),
        ],
        [
            (
                "1  see the group",
                [
                    ("msg", "c", "w", "Discover()", False),
                    ("msg", "w", "c", "Pods, Recreating", True, "grey"),
                    ("msg", "c", "d", "Observe(pods)", False),
                    ("msg", "d", "c", "members per Pod and their state", True),
                ],
            ),
            (
                "2  decide",
                [
                    ("msg", "c", "p", "Decide(input)", False),
                    ("msg", "p", "c", "next action + policy checkpoint", True),
                    (
                        "note",
                        "c",
                        "w",
                        ["a pending operation resumes through Apply", "with the same action, ID and checkpoint"],
                        "amber",
                    ),
                ],
            ),
            (
                "3  save and execute the selected action; Change shown here",
                [
                    ("msg", "c", "j", "Save(operation, policy checkpoint)", False),
                    ("msg", "c", "d", "Apply(observation, operation)", False),
                    ("msg", "d", "c", "status and execution checkpoint", True),
                    ("msg", "c", "j", "Save(report checkpoint)", False),
                ],
            ),
            (
                "4  finish the operation before the next policy decision",
                [
                    ("msg", "c", "j", "Save(result)", False),
                    ("note", "c", "p", ["Decide uses the saved result to choose the next action"], "amber"),
                ],
            ),
        ],
        subtitle="repeated every poll · the journal is saved before anything reaches the engine",
        legend=[
            ("line", "blue", "controller call"),
            ("dash", "green", "engine reply"),
            ("dash", "grey", "workload reply"),
            ("amber", "amber", "rule"),
        ],
    )


def recovery_sequence():
    sequence(
        "recovery-sequence.svg",
        "Who does what when one member dies",
        [
            ("c", "Controller", "blue"),
            ("p", "Engine policy", "green"),
            ("d", "Engine driver", "green"),
            ("w", "Workload", "grey"),
        ],
        [
            (
                "t1–t2  remove the dead member  ·  recovering",
                [
                    ("msg", "c", "d", "Observe(pods)", False),
                    ("msg", "d", "c", "m3 terminal, m0–m2 paused", True),
                    ("msg", "c", "p", "Decide(...)", False),
                    ("msg", "p", "c", "Change: remove m3", True),
                    ("msg", "c", "d", "Apply(remove m3)", False),
                    ("msg", "c", "d", "Apply(same operation)", False),
                    ("msg", "d", "c", "Succeeded", True),
                    ("msg", "c", "p", "Decide(..., last result)", False),
                    ("msg", "p", "c", "Verify: m0, m1, m2", True),
                    ("msg", "c", "d", "Apply(Verify: m0, m1, m2)", False),
                    ("note", "c", "d", ["serving with 3 members; pod-3 holds only an excluded member"], "amber"),
                ],
            ),
            (
                "t3  reset the failed Pod  ·  serving",
                [
                    ("msg", "c", "p", "Decide(..., verification succeeded)", False),
                    ("msg", "p", "c", "ReplaceMembers: m3", True),
                    ("msg", "c", "w", "Reset(pod-3, UID d1)", False),
                    ("msg", "w", "c", "pod-3 back with UID d2, then Ready", True, "grey"),
                ],
            ),
            (
                "t4–t5  admit the new member  ·  serving",
                [
                    ("msg", "c", "d", "Observe(pods)", False),
                    ("msg", "d", "c", "m3 on pod-3 (d2) joinable", True),
                    ("msg", "c", "p", "Decide(...)", False),
                    ("msg", "p", "c", "Change: add m3", True),
                    ("msg", "c", "d", "Apply(add m3)", False),
                    ("msg", "c", "d", "Apply(same operation)", False),
                    ("msg", "d", "c", "Succeeded", True),
                    ("msg", "c", "p", "Decide(..., last result)", False),
                    ("msg", "p", "c", "Verify: m0 through m3", True),
                    ("msg", "c", "d", "Apply(Verify: m0 through m3)", False),
                    ("note", "c", "d", ["serving with 4 members"], "green"),
                ],
            ),
        ],
        subtitle="the same recovery as the timeline above, as calls",
        lane_w=210,
        legend=[
            ("line", "blue", "controller call"),
            ("dash", "green", "engine reply"),
            ("dash", "grey", "workload reply"),
            ("amber", "amber", "group state"),
        ],
    )


def recovery_timeline():
    s = Svg(
        1060,
        0,
        "One member dies: 4 → 3 → 4",
        "four Pods with one member each · an engine that can grow · the group keeps serving the whole time",
    )
    rows = [
        ("t0", "serving", "4 members", "blue", ["active"] * 4, ["serving"] * 4, ()),
        (
            "t1",
            "m3 dies",
            "recovering",
            "amber",
            ["paused", "paused", "paused", "failed"],
            ["paused", "paused", "paused", "dead"],
            ("m3",),
        ),
        (
            "t2",
            "a reconfig removes m3",
            "serving 3",
            "blue",
            ["active", "active", "active", "failed"],
            ["serving", "serving", "serving", "excluded"],
            (),
        ),
        (
            "t3",
            "Reset replaces pod-3",
            "serving 3",
            "blue",
            ["active", "active", "active", "starting"],
            ["serving", "serving", "serving", "new UID, starting"],
            (),
        ),
        (
            "t4",
            "a reconfig adds the new m3",
            "serving 3",
            "blue",
            ["active", "active", "active", "joining"],
            ["serving", "serving", "serving", "joining"],
            (),
        ),
        ("t5", "full again", "4 members", "blue", ["active"] * 4, ["serving"] * 4, ()),
    ]
    y = 96
    for t, what, phase, _, states, status, crossed in rows:
        s.line(28, y - 10, 1032, y - 10, C["rule"], 1.2)
        s.text(32, y + 18, t, 15, color=C["blue"], bold=True)
        s.text(64, y + 18, what, 14, color=C["blue"], bold=True)
        tone = {"recovering": "amber"}.get(phase, "green")
        s.rect(32, y + 34, 9 + width(phase, 12, True) + 9, 22, C[tone], C[tone + "_bg"], r=11, sw=1.2)
        s.text(41, y + 50, phase, 12, color=C[tone], bold=True)
        x = 300
        for i, state in enumerate(states):
            st = status[i]
            pod_state = state
            sub = "UID d2" if t in ("t3", "t4", "t5") and i == 3 else "UID d1" if i == 3 else f"UID {'abc'[i]}1"
            pod(s, x, y, f"pod-{i}", sub, [(f"m{i}", state)], st, pod_state, w=170, crossed=crossed)
            x += 182
        y += 140
    s.line(28, y - 10, 1032, y - 10, C["rule"], 1.2)
    s.legend(
        y + 22,
        [
            ("active", "green", "serving"),
            ("paused", "amber", "paused or joining"),
            ("failed", "red", "dead or excluded"),
            ("starting", "grey", "new Pod, not yet a member"),
        ],
    )
    s.text(
        32,
        y + 52,
        "One cell is one engine member. Pod names and UIDs come from the workload, member IDs from the engine.",
        12,
        color=C["muted"],
    )
    s.h = y + 74
    s.save("recovery-timeline.svg")


def phases():
    s = Svg(1100, 400, "Controller phases", "where the controller is, and what moves it")
    pill = [
        ("starting", 70, "grey", "the group comes up"),
        ("serving", 450, "green", "the group serves"),
        ("recovering", 830, "amber", "a fault is repaired"),
    ]
    for name, x, tone, line in pill:
        s.rect(x, 130, 200, 84, C[tone], C[tone + "_bg"], r=42, sw=2.5)
        s.text(x + 100, 166, name, 19, color=C[tone], bold=True, anchor="middle")
        s.text(x + 100, 190, line, 13, color=C["muted"], anchor="middle")
    s.arrow([(270, 172), (450, 172)], C["green"], "every member is up", (360, 162))
    s.arrow([(650, 158), (830, 158)], C["amber"], "a fault", (740, 148))
    s.arrow([(830, 192), (650, 192)], C["green"], "Decide: Healthy", (740, 212))
    s.arrow([(550, 214), (550, 270), (200, 270), (200, 214)], C["red"])
    s.arrow([(930, 214), (930, 310), (140, 310), (140, 214)], C["red"])
    s.text(375, 262, "Decide: RecreateGroup", 13, color=C["red"], bold=True, anchor="middle")
    s.text(535, 302, "Decide: RecreateGroup", 13, color=C["red"], bold=True, anchor="middle")
    s.legend(
        360,
        [
            ("line", "green", "things get better"),
            ("line", "amber", "a fault"),
            ("line", "red", "a reset: every Pod is recreated"),
        ],
    )
    s.save("phases.svg")


def adopt():
    s = Svg(1040, 470, "Same group or a new one?", "checked on every Discover, before anything else")
    s.box(60, 100, 220, 56, "Discover", [], tone="blue", center=True)
    s.box(
        360,
        92,
        300,
        72,
        "Recreating, stopped, or an owed",
        ["ResetAll with old Pods left?"],
        tone="amber",
        center=True,
        size=13.5,
    )
    s.box(740, 92, 260, 72, "Wait, act on nothing", ["repeat the owed ResetAll"], tone="grey", center=True)
    s.box(
        360,
        250,
        300,
        72,
        "Any saved Pod still there",
        ["with the same identity?"],
        tone="amber",
        center=True,
        size=13.5,
    )
    s.box(740, 210, 260, 72, "Same group", ["compare Pod by Pod"], tone="green", center=True)
    s.box(740, 330, 260, 72, "New group", ["clear membership; retain budget"], tone="red", center=True)
    s.arrow([(280, 128), (360, 128)], C["blue"])
    s.arrow([(660, 128), (740, 128)], C["grey"], "yes", (700, 120))
    s.arrow([(510, 164), (510, 250)], C["blue"], "no", (520, 212), anchor="start")
    s.arrow([(660, 272), (700, 272), (700, 246), (740, 246)], C["green"], "yes", (680, 262))
    s.arrow([(660, 300), (700, 300), (700, 366), (740, 366)], C["red"], "no", (680, 320))
    s.save("adopt.svg")


def capacity():
    s = Svg(
        1100,
        570,
        "Restoring capacity",
        "engine policy requests resources; controller selects the workload operation",
    )
    s.box(
        350,
        92,
        400,
        70,
        "Decide(input)",
        ["members, observation, policy state, result"],
        tone="green",
        center=True,
    )
    cols = [
        ("admit a candidate", 40, "Change: Add", "driver resumes native admission", "green"),
        ("request resources", 390, "ReplaceMembers", "names excluded or rejected members", "amber"),
        ("keep reduced capacity", 740, "Healthy", "controller confirms serving", "grey"),
    ]
    for label, x, action, note, tone in cols:
        s.arrow([(550, 162), (550, 190), (x + 160, 190), (x + 160, 235)], C["blue"])
        s.text(x + 160, 225, label, 12.5, color=C["blue"], bold=True, anchor="middle")
        s.box(x, 235, 320, 72, action, [note], tone=tone, center=True)
    s.box(
        220,
        360,
        660,
        82,
        "Controller selects and saves the workload action",
        ["capacity preference + Grow + safe Pod UIDs", "Reset, Release, ResetAll, or retain reduced capacity"],
        tone="blue",
        center=True,
    )
    s.arrow([(550, 307), (550, 360)], C["blue"])
    s.box(
        220,
        480,
        660,
        56,
        "The saved result feeds the next decision",
        ["every new mutation must fit the durable recovery budget"],
        tone="grey",
        center=True,
    )
    s.arrow([(550, 442), (550, 480)], C["blue"])
    s.save("capacity.svg")


def engine():
    s = Svg(
        1000, 300, "The engine plugin", "policy uses observation, checkpoint and result; driver executes engine actions"
    )
    s.box(40, 130, 180, 64, "Controller", [], tone="blue", center=True)
    s.box(390, 92, 220, 64, "Driver", ["Observe, resumable Apply"], tone="green", center=True)
    s.box(390, 196, 220, 64, "Policy", ["Decide - pure"], tone="green", center=True)
    s.box(760, 92, 200, 64, "Engine runtime", ["FT API"], tone="green", center=True)
    s.arrow(
        [(220, 150), (300, 150), (300, 124), (390, 124)],
        C["blue"],
        "Capabilities, I/O calls",
        (230, 114),
        anchor="start",
    )
    s.arrow([(220, 174), (300, 174), (300, 228), (390, 228)], C["blue"], "state, result", (310, 250), anchor="start")
    s.arrow([(610, 124), (760, 124)], C["green"], "HTTP", (685, 116))
    s.arrow([(500, 156), (500, 196)], C["green"], "private engine data", (510, 182), dash="6 4", anchor="start")
    s.save("engine.svg")


if __name__ == "__main__":
    for draw in (
        architecture,
        step,
        recovery_sequence,
        recovery_timeline,
        phases,
        adopt,
        capacity,
        engine,
    ):
        draw()
