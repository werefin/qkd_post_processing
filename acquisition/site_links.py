'''
Links of config.json: every pair of sites is a link, so A_D works as well as C_A; "links" is optional and only
names pairs or fixes which site is A, and a name X_Y not listed there is site X with site Y
'''

from itertools import combinations


def all_links(cfg: dict) -> dict:
    '''Every link of the config, name: {site_a, site_b}; the listed ones first, then each other pair of sites
    under its sorted name'''
    out = {name: {'site_a': link['site_a'], 'site_b': link['site_b']} for name, link in (cfg.get('links') or {}).items()}
    covered = {frozenset((link['site_a'], link['site_b'])) for link in out.values()}
    for a, b in combinations(sorted(cfg.get('sites') or {}), 2):
        if frozenset((a, b)) not in covered:
            out[f'{a}_{b}'] = {'site_a': a, 'site_b': b}
    return out


def link_sites(cfg: dict, name: str) -> tuple:
    '''The two sites of link name, (site_a, site_b), or ValueError naming every link there is'''
    sites = cfg.get('sites') or {}
    listed = (cfg.get('links') or {}).get(name)
    if listed:
        return listed['site_a'], listed['site_b']
    a, sep, b = str(name or '').partition('_')
    if sep and a in sites and b in sites and a != b:
        return a, b
    raise ValueError(f'link {name!r} is not two sites of the config: known links {", ".join(sorted(all_links(cfg)))}')


def check(cfg: dict) -> str:
    '''What is wrong with the config's sites and links, or None'''
    sites = cfg.get('sites')
    if not isinstance(sites, dict) or not sites:
        return 'missing or empty "sites"'
    links = cfg.get('links', {})
    if not isinstance(links, dict):
        return '"links" must map names to {"site_a", "site_b"}'
    for name, link in links.items():
        for key in ('site_a', 'site_b'):
            if (link or {}).get(key) not in sites:
                return f'links.{name}.{key} not found in "sites"'
        if link['site_a'] == link['site_b']:
            return f'links.{name} joins site {link["site_a"]} to itself'
    default = (cfg.get('record_dual') or {}).get('link')
    if default is not None:
        try:
            link_sites(cfg, default)
        except ValueError as e:
            return f'record_dual.link: {e}'
    return None
