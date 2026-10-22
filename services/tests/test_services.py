'''
Node services: each one's routes match its OpenAPI file, the web proxy reaches every /api route and streams through,
the service token guards the inner services, and the agent's scheduling and controller settings behave
'''

import io
import json
import os
import re
import tempfile
import threading
import time
import unittest
from unittest import mock

_DATA = tempfile.mkdtemp(prefix='qkd_services_test_')
for _env in ('KEYSTORE_DIR', 'RECORDINGS_DIR', 'CONFIG_DIR', 'IMPORT_DIR', 'CONTINUOUS_DIR', 'EVAL_DIR', 'CLOG_DIR'):
    os.environ[_env] = os.path.join(_DATA, _env.lower())
os.environ['SERVICE'] = 'all'

from werkzeug.serving import make_server  # noqa: E402

from services.core import settings  # noqa: E402
from services.serve import SERVICES, create_app  # noqa: E402
from services.web import proxy  # noqa: E402

API_DIR = settings.REPO_ROOT / 'api'


def routes(app):
    '''(method, path) of an app, paths written the OpenAPI way'''
    out = set()
    for r in app.url_map.iter_rules():
        if r.endpoint == 'static':
            continue
        path = re.sub(r'<(?:\w+:)?(\w+)>', r'{\1}', r.rule)
        out |= {(m.lower(), path) for m in r.methods - {'HEAD', 'OPTIONS'}}
    return out


def spec_routes(name):
    '''(method, path) of an OpenAPI file, read by indentation: paths at two spaces, methods at four'''
    out, path = set(), None
    for line in (API_DIR / f'{name}.yaml').read_text().splitlines():
        if m := re.match(r'^  (/\S*):$', line):
            path = m[1]
        elif (m := re.match(r'^    (get|post|put|delete|patch):', line)) and path:
            out.add((m[1], path))
        elif re.match(r'^\S', line):
            path = None
    return out


class Serve:
    '''One service app on a free local port, in a thread'''

    def __init__(self, name):
        self.server = make_server('127.0.0.1', 0, create_app(name, start=False), threaded=True)
        self.url = f'http://127.0.0.1:{self.server.server_port}'
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()


class TestRoutes(unittest.TestCase):
    def test_openapi_matches_every_service(self):
        for name in ('agent', 'acquisition', 'processing'):
            with self.subTest(service=name):
                self.assertEqual(routes(create_app(name, start=False)), spec_routes(name))

    def test_proxy_owns_every_api_route_of_its_service(self):
        for name in SERVICES[:3]:
            for _, path in routes(create_app(name, start=False)):
                if path.startswith('/api/'):
                    self.assertEqual(proxy.owner(path.replace('{', '').replace('}', '')), name, path)
        self.assertIsNone(proxy.owner('/api/statusx'))

    def test_all_in_one_serves_the_page_and_every_route(self):
        app = create_app('all', start=False)
        every = set().union(*(routes(create_app(n, start=False)) for n in SERVICES[:3]))
        self.assertTrue(every <= routes(app))
        self.assertEqual(app.test_client().get('/').status_code, 200)


class TestToken(unittest.TestCase):
    def test_inner_services_need_the_token(self):
        with mock.patch.object(settings, 'SERVICE_TOKEN', 'secret'):
            c = create_app('acquisition', start=False).test_client()
            self.assertEqual(c.get('/api/recordings').status_code, 403)
            self.assertEqual(c.get('/api/recordings', headers={'X-Service-Token': 'secret'}).status_code, 200)
            for open_path in ('/healthz', '/readyz', '/metrics'):
                self.assertEqual(c.get(open_path).status_code, 200, open_path)
            # the page's front end answers the browser, which never has the token
            self.assertEqual(create_app('web', start=False).test_client().get('/').status_code, 200)


class TestProxy(unittest.TestCase):
    def setUp(self):
        self.acq = Serve('acquisition')
        self.env = mock.patch.dict(os.environ, {'ACQUISITION_URL': self.acq.url})
        self.env.start()
        self.web = create_app('web', start=False).test_client()

    def tearDown(self):
        self.env.stop()
        self.acq.close()

    def test_json_upload_and_download_pass_through(self):
        with mock.patch.object(settings, 'SERVICE_TOKEN', 'secret'):
            r = self.web.get('/api/recordings?x=1')
            self.assertEqual(r.status_code, 200, r.data)
            self.assertIn('pairs', r.get_json())
            body = os.urandom(300_000)
            name = '20260101_000000_A_p1_meta.json'
            r = self.web.post('/api/upload', data={'files': (io.BytesIO(body), name)}, content_type='multipart/form-data')
            self.assertEqual(r.get_json()['saved'], [name])
            self.assertEqual((settings.RECORDINGS_DIR / name).read_bytes(), body)
            r = self.web.delete('/api/recordings/20260101_000000_A_p1')
            self.assertEqual(r.status_code, 404)  # no timestamp file: refused by the service, passed on as is
            (settings.RECORDINGS_DIR / name).unlink()

    def test_unknown_or_down(self):
        self.assertEqual(self.web.get('/api/nothing').status_code, 404)
        with mock.patch.dict(os.environ, {'PROCESSING_URL': 'http://127.0.0.1:9'}):
            self.assertEqual(self.web.get('/api/status').status_code, 502)


class TestAgent(unittest.TestCase):
    def setUp(self):
        from services.agent import state
        self.state = state
        settings.TP_STATE.unlink(missing_ok=True)

    def test_scheduler_starts_blocks_once_their_first_segment_is_written(self):
        from services.agent import twoparty
        from services.core import peers
        now = 1_000_000.0
        st = {**self.state.DEFAULTS, 'active': True, 'announce_to': ['B'], 'block_s': 100.0, 'segment_s': 10.0,
              'block_auto': False, 'next_block': {'B': 999_800.0}}
        sent = []
        with mock.patch.object(time, 'time', return_value=now), \
                mock.patch.object(peers, 'call_json', side_effect=lambda *a, **k: sent.append((a[3]['block'], a[3]['segment_s']))):
            self.assertTrue(twoparty._queue_due(st))
        # the block in progress started at 999 900 and its first segment is written: it streams from now on
        self.assertEqual(sent, [(999_800.0, 10.0), (999_900.0, 10.0)])
        self.assertEqual(st['next_block']['B'], 1_000_000.0)
        st['next_block']['B'] = 999_800.0
        with mock.patch.object(time, 'time', return_value=now), \
                mock.patch.object(peers, 'call_json', side_effect=peers.PeerError('down')):
            twoparty._queue_due(st)
        self.assertEqual(st['next_block']['B'], 999_800.0)  # nothing is lost while processing is down
        st['next_block']['B'] = 999_000.0
        with mock.patch.object(time, 'time', return_value=now), \
                mock.patch.object(peers, 'call_json', side_effect=lambda *a, **k: None):
            twoparty._queue_due(st)
        self.assertEqual(st['next_block']['B'], 1_000_080.0)  # too old for the kept segments: from the newest one

    def test_block_length_planned_from_the_link_record(self):
        from services.agent import blockplan, twoparty
        from services.core import peers
        st = {**self.state.DEFAULTS, 'active': True, 'announce_to': ['B'], 'block_s': 900.0, 'segment_s': 5.0}
        with mock.patch.object(peers, 'call_json', return_value={'blocks': []}):
            self.assertEqual(blockplan.length(st, 'B'), (300.0, None))  # nothing to plan from: the first length
        done = [{'role': 'alice', 'ok': True, 'sifted': 90_000, 'seconds': 300.0, 'block_s': 300.0, 'qber': q,
                 'ec_leak': int(1.05 * 90_000 * blockplan._h2(q))} for q in (0.02, 0.02)]
        rec = blockplan.record(done + [{'role': 'alice', 'ok': False, 'block_s': 300.0}, {'role': 'bob', 'ok': False}])
        self.assertAlmostEqual(rec['sifted_per_s'], 300.0)
        self.assertAlmostEqual(rec['ec_efficiency'], 1.05, places=3)
        self.assertAlmostEqual(rec['fail_per_s'], 1 / (900 + blockplan.PRIOR_S))
        plan = {'block_s': 405, 'rate': 105.9, 'best_s': 900, 'best_rate': 111.4}
        run = mock.Mock(return_value=mock.Mock(stdout=json.dumps(plan) + '\n'))
        with mock.patch.object(peers, 'call_json', return_value={'blocks': done}), mock.patch.object(blockplan.subprocess, 'run', run):
            self.assertEqual(blockplan.length(st, 'B')[0], 405.0)
        self.assertEqual(json.loads(run.call_args[0][0][2])['max_s'], 900.0)
        sent = []
        with mock.patch.object(time, 'time', return_value=1_000_000.0), \
                mock.patch.object(blockplan, 'length', return_value=(405.0, plan)), \
                mock.patch.object(peers, 'call_json', side_effect=lambda *a, **k: sent.append(a[3]['block_s'])):
            st['next_block'] = {'B': 999_500.0}
            twoparty._queue_due(st)
        self.assertEqual(sent, [405.0, 405.0])  # the second starts at 999 905, its first segment already written
        self.assertEqual((st['next_block']['B'], st['plans']['B']['block_s']), (1_000_310.0, 405.0))

    def test_controller_settings_apply_and_release(self):
        from services.agent import controller, twoparty
        follow = mock.Mock(return_value=(200, {}))
        with mock.patch.object(twoparty, 'follow', follow):
            controller.apply({'managed': True, 'active': True, 'site': 'A', 'announce_to': ['B'], 'segment_s': 10,
                              'block_s': 300, 'expected_qber': 0.05, 'keep_blocks': 2})
            st = self.state.load()
            self.assertTrue(st['managed'] and st['active'])
            self.assertEqual(st['announce_to'], ['B'])
            self.assertEqual(follow.call_count, 1)
            controller.apply({'managed': True, 'active': True, 'site': 'A', 'announce_to': ['B'], 'segment_s': 10,
                              'block_s': 300, 'expected_qber': 0.05, 'keep_blocks': 2})
            self.assertEqual(follow.call_count, 1)  # the same settings again change nothing
            r = create_app('agent', start=False).test_client().post('/api/twoparty/stop')
            self.assertEqual(r.status_code, 409)  # a managed node is changed at the controller
            controller.apply({'managed': False})
            self.assertFalse(self.state.load()['managed'])


class TestTagger(unittest.TestCase):
    def test_continuous_qkd_holds_the_timetagger_from_start_to_stop(self):
        from services.acquisition import recorder, tagger
        recorder._wanted_file().unlink(missing_ok=True)
        self.assertIsNone(tagger.busy())
        recorder._wanted_file().parent.mkdir(parents=True, exist_ok=True)
        recorder._wanted_file().touch()  # what a start leaves, even when the recorder itself is down
        self.assertEqual(tagger.busy(), 'continuous QKD')
        self.assertIsNone(tagger.busy('the two-party recorder', 'continuous QKD'))  # the recorder may still start
        client = create_app('acquisition', start=False).test_client()
        self.assertEqual(client.post('/v1/recorder/stop', headers={'X-Service-Token': settings.SERVICE_TOKEN}).status_code, 200)
        self.assertIsNone(tagger.busy())

    def test_one_mode_at_a_time(self):
        from services.acquisition import tagger
        with mock.patch.dict(tagger._modes, {'x': lambda: True}):
            self.assertEqual(tagger.busy(), 'x')
            self.assertIsNone(tagger.busy('x'))


if __name__ == '__main__':
    unittest.main()

