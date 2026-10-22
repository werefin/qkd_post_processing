'''
Running the pipeline's programs: long-lived acquisition processes read line by line, and jobs whose stderr goes
to a log while their stdout carries the JSON result
'''

import json
import subprocess
import threading
from pathlib import Path

from . import settings


def start(args, on_line, on_exit=None):
    '''Starts a process in acquisition/ and hands each output line to on_line from a reader thread'''
    proc = subprocess.Popen(args, cwd=str(settings.ACQUISITION_DIR), stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, bufsize=1)

    def read():
        for line in proc.stdout:
            on_line(line.rstrip('\n'))
        proc.wait()
        if on_exit:
            on_exit()
    threading.Thread(target=read, daemon=True).start()
    return proc


def running(proc):
    return proc is not None and proc.poll() is None


def prefixed_json(line, prefix):
    '''The JSON after a status line's prefix, or None'''
    if not line.startswith(prefix):
        return None
    try:
        return json.loads(line[len(prefix):])
    except ValueError:
        return None


def last_json(text):
    '''A command's result: the JSON on its last output line'''
    return json.loads(text.strip().splitlines()[-1])


def run_logged(log, args, timeout=3600):
    '''Runs a command, streaming its stderr into log; returns its stdout'''
    log.append('$ ' + ' '.join([Path(args[0]).name] + args[1:]))
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    out = []
    reader = threading.Thread(target=lambda: out.append(proc.stdout.read()), daemon=True)
    reader.start()
    killer = threading.Timer(timeout, proc.kill)
    killer.start()
    for line in proc.stderr:
        log.append(line.rstrip('\n'))
    proc.wait()
    killer.cancel()
    reader.join()
    if proc.returncode != 0:
        raise RuntimeError(f'exit status {proc.returncode}: ' + ' / '.join(list(log)[-3:]))
    return out[0] if out else ''
