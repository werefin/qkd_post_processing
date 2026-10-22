'''
Acquisition config on the data volume, as the acquisition service stored it; read by every service that needs a
site's settings
'''

import json

from . import settings


def load():
    '''Loaded config, or {} when none is'''
    p = settings.UPLOADED_CONFIG_PATH
    return json.loads(p.read_text()) if p.is_file() else {}


def loaded():
    return settings.UPLOADED_CONFIG_PATH.is_file()
