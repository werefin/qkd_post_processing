'''
Coincidence logging: options, the plan of a log, counting, clock alignment, sources and outputs, and the run
tying them together; coincidence_logger.py is the command line
'''

from .align import LiveModel, OneClock
from .counting import PS_PER_S, WindowCounter
from .options import DEFAULTS, merge
from .outputs import STATUS_PREFIX, CsvLog, StatusLines, read_log
from .plan import FORMAT, Plan, compile_expr
from .run import Run
from .sources import FileSource, TaggerSource
