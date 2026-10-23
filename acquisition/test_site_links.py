'''
Links of config.json: every pair of sites is a link without listing it, the listed links keep their names and
direction, X_Y works either way round, and a link the sites cannot make is refused
run: python3 -m unittest acquisition/test_site_links.py
'''

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import site_links  # noqa: E402

SITES = {s: {} for s in 'ABCD'}


class SiteLinksTest(unittest.TestCase):
    def test_every_pair_without_links(self):
        links = site_links.all_links({'sites': SITES})
        self.assertEqual(sorted(links), ['A_B', 'A_C', 'A_D', 'B_C', 'B_D', 'C_D'])
        self.assertEqual(links['A_D'], {'site_a': 'A', 'site_b': 'D'})

    def test_listed_links_kept(self):
        cfg = {'sites': SITES, 'links': {'C_A': {'site_a': 'C', 'site_b': 'A'}, 'D_A': {'site_a': 'D', 'site_b': 'A'}}}
        links = site_links.all_links(cfg)
        self.assertEqual(len(links), 6)
        self.assertNotIn('A_C', links)
        self.assertIn('B_D', links)
        self.assertEqual(site_links.link_sites(cfg, 'C_A'), ('C', 'A'))

    def test_either_direction(self):
        cfg = {'sites': SITES}
        self.assertEqual(site_links.link_sites(cfg, 'A_D'), ('A', 'D'))
        self.assertEqual(site_links.link_sites(cfg, 'D_A'), ('D', 'A'))

    def test_refused(self):
        cfg = {'sites': SITES}
        for name in ('A_X', 'A_A', 'AD', None):
            with self.assertRaises(ValueError):
                site_links.link_sites(cfg, name)
        self.assertIn('not found', site_links.check({'sites': SITES, 'links': {'x': {'site_a': 'A', 'site_b': 'E'}}}))
        self.assertIn('itself', site_links.check({'sites': SITES, 'links': {'x': {'site_a': 'A', 'site_b': 'A'}}}))
        self.assertIn('record_dual.link', site_links.check({'sites': SITES, 'record_dual': {'link': 'A_E'}}))
        self.assertIsNone(site_links.check({'sites': SITES, 'record_dual': {'link': 'B_D'}}))


if __name__ == '__main__':
    unittest.main()
