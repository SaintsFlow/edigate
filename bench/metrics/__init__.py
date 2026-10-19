"""Metrics of the benchmark (protocol section 7): oracles, utility, duplicate rates, summary tables.

Everything here works on a plain dict called the state (see snapshot.py for its shape) and on the
label of the document, so the rules are testable without a database. Only snapshot.py talks to PostgreSQL.
"""
