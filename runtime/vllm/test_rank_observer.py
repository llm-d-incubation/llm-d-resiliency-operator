# Copyright 2026 The llm-d Authors.
# SPDX-License-Identifier: Apache-2.0
from rank_observer import ProcessObserver


def processes():
    return [
        {"name": name, "pid": pid, "create_time": 1.0, "status": "running"}
        for pid, name in enumerate(["VLLM::APIServer_DP1", "VLLM::EngineCore_DP1", "VLLM::Worker_DP1_EP1"], 10)
    ]


def test_original_rank_layout_is_available_before_processes_start():
    observer = ProcessObserver("pod-a", [2, 3])
    assert [rank["id"] for rank in observer.snapshot()["ranks"]] == [2, 3]
    observer.sample([])
    assert [rank["id"] for rank in observer.snapshot()["ranks"]] == [2, 3]
    assert not any(rank["engine_dead"] for rank in observer.snapshot()["ranks"])


def test_api_exit_is_not_engine_death():
    observer = ProcessObserver("pod-a", [1])
    observer.sample(processes())
    observer.sample(processes()[1:])
    rank = observer.snapshot()["ranks"][0]
    assert not rank["engine_dead"]
    assert not rank["processes"]["api"]["alive"]
    assert rank["processes"]["engine"]["alive"]


def test_engine_exit_survives_missing_api_and_cannot_rejoin():
    observer = ProcessObserver("pod-a", [1])
    observer.sample(processes())
    observer.sample(processes()[2:])
    assert observer.snapshot()["ranks"][0]["engine_dead"]
    replacement = processes()
    replacement[1]["create_time"] = 2.0
    observer.sample(replacement)
    assert observer.snapshot()["ranks"][0]["engine_dead"]
    assert observer.snapshot()["pod_uid"] == "pod-a"


def test_not_started_and_zombie_processes_are_distinct():
    observer = ProcessObserver("pod-a", [1])
    observer.sample([])
    assert not observer.snapshot()["ranks"][0]["engine_dead"]
    observer.sample(processes())
    rows = processes()
    rows[2]["status"] = "zombie"
    observer.sample(rows)
    assert observer.snapshot()["ranks"][0]["engine_dead"]


def test_unreadable_process_is_not_reported_dead():
    observer = ProcessObserver("pod-a", [1])
    observer.sample(processes())
    rows = processes()
    rows[2]["name"] = None
    observer.sample(rows)
    assert not observer.snapshot()["ranks"][0]["engine_dead"]
