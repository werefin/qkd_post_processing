'''
Settings of every service, from the environment: the shared data directories, the pipeline binaries, this node,
its key management system, the network controller, and where the other services listen
'''

import os
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
UI_DIR = REPO_ROOT / 'ui'
ACQUISITION_DIR = REPO_ROOT / 'acquisition'


def _binary(env, name):
    '''A binary from env, else one built at the repository root, else a PATH lookup'''
    raw = os.environ.get(env) or (str(REPO_ROOT / name) if (REPO_ROOT / name).is_file() else name)
    return str(Path(raw).resolve()) if os.sep in raw else raw


def _dir(env, default):
    '''A data directory, made if it can be; a service that does not mount it never touches it'''
    p = Path(os.environ.get(env) or default).resolve()
    try:
        p.mkdir(parents=True, exist_ok=True)
    except OSError:
        pass
    return p


QKD_BIN = _binary('QKD_BIN', 'qkd_post_processing')
IMPORTTT_BIN = _binary('IMPORTTT_BIN', 'importtt')

# Simulator keys are demo material and live apart from IMPORT_DIR, which the KMS imports from
KEYSTORE_DIR = _dir('KEYSTORE_DIR', UI_DIR / 'keys')
RECORDINGS_DIR = _dir('RECORDINGS_DIR', UI_DIR / 'recordings')
CONFIG_DIR = _dir('CONFIG_DIR', UI_DIR / 'config')
IMPORT_DIR = _dir('IMPORT_DIR', UI_DIR / 'import')
# continuous-QKD runs and evaluation reports: made from real data, so kept on this node only
CONTINUOUS_DIR = _dir('CONTINUOUS_DIR', UI_DIR / 'continuous')
EVAL_DIR = _dir('EVAL_DIR', UI_DIR / 'evaluations')
CLOG_DIR = _dir('CLOG_DIR', UI_DIR / 'coincidence_logs')
UPLOADED_CONFIG_PATH = CONFIG_DIR / 'config.json'
DELAYS_PATH = CONFIG_DIR / 'detector_delays.json'
JOBS_PATH = CONFIG_DIR / 'jobs.jsonl'

# Continuous two-party QKD: the recorder writes segments, the block work reads them and logs every block
TP_DIR = _dir('TWO_PARTY_DIR', CONTINUOUS_DIR / 'two-party')
TP_SEGMENTS = TP_DIR / 'segments'
TP_WORK = TP_DIR / 'work'
TP_BLOCKS = TP_DIR / 'blocks.jsonl'
TP_MODELS = TP_DIR / 'models.json'
TP_RECORDER_PID = TP_DIR / 'recorder.pid'
TP_STATE = CONFIG_DIR / 'two_party.json'

KEYSTORE_MAX = int(os.environ.get('KEYSTORE_MAX', '30000'))
UPLOAD_MAX_MB = int(os.environ.get('UPLOAD_MAX_MB', '102400'))
# raw captures are irreplaceable: never deleted automatically unless a cap is set
RECORDINGS_MAX_GB = float(os.environ.get('RECORDINGS_MAX_GB', '0'))

NODE_ID = os.environ.get('NODE_ID', '')
KMS_URL = os.environ.get('KMS_URL', '').rstrip('/')
KMS_CA = os.environ.get('KMS_CA', '')
KMS_IDENTITIES = {
    'admin': (os.environ.get('KMS_ADMIN_CERT', ''), os.environ.get('KMS_ADMIN_KEY', '')),
    'sae': (os.environ.get('KMS_SAE_CERT', ''), os.environ.get('KMS_SAE_KEY', '')),
}
SAE_ID = f'dashboard-{NODE_ID}' if NODE_ID else ''
TWO_PARTY_AUTO_ACCEPT = os.environ.get('TWO_PARTY_AUTO_ACCEPT', '1') == '1'

# the network controller, reached with this node's admin certificate; unset, the node runs on its own settings
CONTROLLER_URL = os.environ.get('CONTROLLER_URL', '').rstrip('/')
CONTROLLER_CA = os.environ.get('CONTROLLER_CA', '') or KMS_CA
CONTROLLER_INTERVAL_S = float(os.environ.get('CONTROLLER_INTERVAL_S', '10'))

# alerts of continuous two-party QKD: posted to the webhook when they change, if one is set
ALERT_WEBHOOK = os.environ.get('ALERT_WEBHOOK', '')
ALERT_MIN_KEYS = int(os.environ.get('ALERT_MIN_KEYS', '100'))
ALERT_MAX_QBER = float(os.environ.get('ALERT_MAX_QBER', '0.11'))

# Services: which one this process runs, the port each listens on, and the token they expect from each other
PORTS = {'agent': 9100, 'acquisition': 9101, 'processing': 9102, 'web': 9823, 'all': 9823}
SERVICE = os.environ.get('SERVICE', 'all')
PORT = int(os.environ.get('PORT') or PORTS.get(SERVICE, 9823))
SERVICE_TOKEN = os.environ.get('SERVICE_TOKEN', '')


def service_url(name):
    '''Where another service listens: <NAME>_URL, else this process when it runs them all, else this host'''
    env = os.environ.get(f'{name.upper()}_URL')
    if env:
        return env.rstrip('/')
    return f'http://127.0.0.1:{PORT if SERVICE == "all" else PORTS[name]}'


# the acquisition scripts (coincidence_peak.py and friends) run in these processes, the same code the CLIs use
if str(ACQUISITION_DIR) not in sys.path:
    sys.path.insert(0, str(ACQUISITION_DIR))
