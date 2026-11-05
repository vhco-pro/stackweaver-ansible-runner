# Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.
"""Stackweaver's stdout callback: one JSON object per line for every play, task, host result and
the recap, which the Stackweaver API stores as the job's events.

The executor writes this file into each job and points ANSIBLE_CALLBACK_PLUGINS at it, so a job
in any image, an execution environment without ansible.posix included, reports the same events.
The line format is the one the API ingests: ``_event`` names the callback, ``_timestamp`` is UTC,
a result line carries the task and ``hosts[<name>]`` with the module result.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = """
    name: stackweaver_jsonl
    short_description: Job events as JSON lines for Stackweaver
    description:
        - Writes every play, task, host result and the recap to stdout as one JSON object per line.
    type: stdout
    requirements:
      - Set as the stdout callback
"""

import copy
import datetime
import json

from ansible.module_utils.common.text.converters import to_text
from ansible.parsing.ajson import AnsibleJSONEncoder
from ansible.plugins.callback import CallbackBase

# Strategies that run a task on every host before the next task starts: a task's start is one
# event for all hosts. Under any other strategy each host starts a task on its own.
_LOCKSTEP = frozenset(("linear", "debug"))


def _now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


class CallbackModule(CallbackBase):
    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = "stdout"
    CALLBACK_NAME = "stackweaver_jsonl"

    def __init__(self, display=None):
        super(CallbackModule, self).__init__(display)
        self._play = None
        self._task = None
        self._per_host = {}
        self._lockstep = True

    def _emit(self, event, doc):
        doc["_event"] = event
        doc["_timestamp"] = _now()
        self._display.display(json.dumps(doc, cls=AnsibleJSONEncoder, separators=(",", ":"), sort_keys=True))

    @staticmethod
    def _describe(obj):
        return {
            "name": obj.get_name(),
            "id": to_text(obj._uuid),
            "path": to_text(obj.get_path()),
            "duration": {"start": _now()},
        }

    def v2_playbook_on_play_start(self, play):
        self._lockstep = play.strategy in _LOCKSTEP
        self._play = {"play": self._describe(play), "tasks": []}
        self._emit("v2_playbook_on_play_start", self._play)

    def _task_started(self, event, task):
        doc = {"task": self._describe(task), "hosts": {}}
        self._task = doc
        self._emit(event, doc)
        return doc

    def v2_playbook_on_task_start(self, task, is_conditional):
        if self._lockstep:
            self._task_started("v2_playbook_on_task_start", task)

    def v2_playbook_on_handler_task_start(self, task):
        if self._lockstep:
            self._task_started("v2_playbook_on_handler_task_start", task)

    def v2_runner_on_start(self, host, task):
        if not self._lockstep:
            self._per_host[(host.get_name(), task._uuid)] = self._task_started("v2_runner_on_start", task)

    def _result(self, event, result, flags):
        host, task = result._host, result._task
        key = (host.get_name(), task._uuid)
        started = self._per_host.pop(key, None) or self._task or {"task": self._describe(task), "hosts": {}}
        end = _now()
        started["task"]["duration"]["end"] = end
        if self._play is not None:
            self._play["play"]["duration"]["end"] = end
        doc = copy.deepcopy(started)
        outcome = dict(result._result)
        outcome.update(flags)
        outcome["action"] = task.action
        doc["hosts"] = {host.get_name(): outcome}
        self._emit(event, doc)

    def v2_runner_on_ok(self, result):
        self._result("v2_runner_on_ok", result, {})

    def v2_runner_on_failed(self, result, ignore_errors=False):
        self._result("v2_runner_on_failed", result, {"failed": True})

    def v2_runner_on_skipped(self, result):
        self._result("v2_runner_on_skipped", result, {"skipped": True})

    def v2_runner_on_unreachable(self, result):
        self._result("v2_runner_on_unreachable", result, {})

    def v2_playbook_on_stats(self, stats):
        summary = {}
        for name in sorted(stats.processed.keys()):
            summary[name] = stats.summarize(name)
        self._emit("v2_playbook_on_stats", {"stats": summary, "custom_stats": {}, "global_custom_stats": {}})
