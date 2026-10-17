"""Draws the vLLM figure: python3 docs/figures/vllm.py"""

from figures import C, Svg


def vllm_states():
    s = Svg(1080, 420, "A vLLM rank's fault-tolerance state", "as reported by /v1/fault_tolerance/status")
    nodes = {
        "healthy": (60, 120, "green"),
        "diagnosing": (420, 120, "amber"),
        "unhealthy": (780, 120, "amber"),
        "dead": (420, 300, "red"),
    }
    for name, (x, y, tone) in nodes.items():
        s.rect(x, y, 220, 56, C[tone], C[tone + "_bg"], r=28, sw=2.5)
        s.text(x + 110, y + 34, name, 17, color=C[tone], bold=True, anchor="middle")
    s.arrow([(280, 140), (420, 140)], C["amber"], "fault in the busy loop", (350, 130))
    s.arrow([(640, 140), (780, 140)], C["amber"], "peers' mask known", (710, 130))
    s.arrow([(890, 120), (890, 84), (170, 84), (170, 120)], C["green"], "retry or scale_down succeeded", (530, 76))
    s.arrow([(170, 176), (170, 328), (420, 328)], C["red"], "own worker died", (180, 318), anchor="start")
    s.arrow([(530, 176), (530, 300)], C["red"], "own worker died", (540, 248), anchor="start")
    s.arrow(
        [(890, 176), (890, 328), (640, 328)],
        C["red"],
        ["scale_down failed, or no instruction", "for engine_recovery_timeout_sec"],
        (880, 222),
        anchor="end",
    )
    s.save("vllm-states.svg")


if __name__ == "__main__":
    vllm_states()
