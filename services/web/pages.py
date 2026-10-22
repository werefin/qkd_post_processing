'''
Dashboard page; its script and styles come from ui/static
'''

from flask import Blueprint, render_template

bp = Blueprint('web_pages', __name__)


@bp.route('/')
def index():
    return render_template('index.html')
