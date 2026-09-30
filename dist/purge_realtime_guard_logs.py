import sqlite3
db = "/opt/cli-proxy-api/plugin-data/cpa-xai-ip-switcher/ip-switcher.sqlite3"
conn = sqlite3.connect(db)
before = conn.execute("SELECT COUNT(*) FROM plugin_logs WHERE category='realtime_guard'").fetchone()[0]
conn.execute("DELETE FROM plugin_logs WHERE category='realtime_guard'")
conn.commit()
after = conn.execute("SELECT COUNT(*) FROM plugin_logs WHERE category='realtime_guard'").fetchone()[0]
print("realtime_guard_logs_before=%s" % before)
print("realtime_guard_logs_after=%s" % after)
conn.close()