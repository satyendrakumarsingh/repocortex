"""The negative fixture: a cache with a database-shaped vocabulary.

It queries, it adds, it deletes, and it imports no ORM. Every name here is one
the SQLAlchemy extractor also looks for, which is exactly why the file exists:
the gate is the import, not the vocabulary.
"""

import json


class Cache:
    __tablename__ = "not_a_table"

    def __init__(self):
        self.session = {}

    def query(self, key):
        return self.session.get(key)

    def add(self, key, value):
        self.session[key] = value

    def delete(self, key):
        self.session.pop(key, None)

    def dump(self):
        return json.dumps(self.session)
