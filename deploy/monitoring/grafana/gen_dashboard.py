import json
DS={"type":"prometheus","uid":"haify-prom"}
C='cluster="$cluster"'
panels=[]; nid=[0]
def nxt(): nid[0]+=1; return nid[0]
def tgt(expr, ref, legend="", table=False):
    t={"refId":ref,"expr":expr,"datasource":DS,"legendFormat":legend}
    if table: t.update({"format":"table","instant":True,"range":False})
    return t
def row(title,y): panels.append({"id":nxt(),"type":"row","title":title,"collapsed":False,"gridPos":{"x":0,"y":y,"w":24,"h":1},"panels":[]})
def steps(*pairs): return {"mode":"absolute","steps":[{"color":c,"value":v} for c,v in pairs]}
def stat(title, expr, x, y, w=4, h=4, unit=None, th=None, mappings=None, nodata="0"):
    d={"thresholds":th or steps(("green",None)),"noValue":nodata}
    if unit: d["unit"]=unit
    if mappings: d["mappings"]=mappings
    panels.append({"id":nxt(),"type":"stat","title":title,"datasource":DS,"gridPos":{"x":x,"y":y,"w":w,"h":h},
        "targets":[tgt(expr,"A")],"fieldConfig":{"defaults":d,"overrides":[]},
        "options":{"colorMode":"background","graphMode":"none","textMode":"value","justifyMode":"center","reduceOptions":{"calcs":["lastNotNull"],"fields":"","values":False}}})
def vmap(m): return [{"type":"value","options":{str(k):{"text":t,"color":c,"index":i} for i,(k,(t,c)) in enumerate(m.items())}}]
def table(title, x, y, w, h, targets, join, rename, order, overrides, nodata="—"):
    exclude={"Time":True}
    for t in targets: exclude[f"Time {t['refId']}"]=True
    panels.append({"id":nxt(),"type":"table","title":title,"datasource":DS,"gridPos":{"x":x,"y":y,"w":w,"h":h},
        "targets":targets,
        "transformations":[{"id":"joinByField","options":{"byField":join,"mode":"outer"}},
            {"id":"organize","options":{"excludeByName":exclude,"renameByName":rename,"indexByName":{k:i for i,k in enumerate(order)}}}],
        "fieldConfig":{"defaults":{"noValue":nodata,"custom":{"align":"left","cellOptions":{"type":"auto"}}},"overrides":overrides},
        "options":{"showHeader":True,"cellHeight":"sm","sortBy":[{"displayName":order[0],"desc":False}]}})
def col(name, props): return {"matcher":{"id":"byName","options":name},"properties":props}
def colored(m): return [{"id":"mappings","value":vmap(m)},{"id":"custom.cellOptions","value":{"type":"color-text"}}]
def ts(title, targets, x, y, w, h, unit=None, stack=False, th=None, maxv=None, nodata=None, overrides=None):
    d={"custom":{"drawStyle":"line","lineWidth":2,"fillOpacity":10 if not stack else 60,"showPoints":"never","stacking":{"mode":"normal" if stack else "none"},"spanNulls":True}}
    if unit: d["unit"]=unit
    if th: d["thresholds"]=th; d["custom"]["thresholdsStyle"]={"mode":"line+area" if False else "line"}
    if maxv is not None: d["max"]=maxv; d["min"]=0
    if nodata: d["noValue"]=nodata
    panels.append({"id":nxt(),"type":"timeseries","title":title,"datasource":DS,"gridPos":{"x":x,"y":y,"w":w,"h":h},
        "targets":targets,"fieldConfig":{"defaults":d,"overrides":overrides or []},
        "options":{"legend":{"displayMode":"table","placement":"right","calcs":["lastNotNull","max"]},"tooltip":{"mode":"multi","sort":"desc"}}})

ok_bad=steps(("green",None),("red",1))
y=0
stat("Controller", f'up{{job="haify",{C}}}',0,y,4,4,th=steps(("red",None),("green",1)),mappings=vmap({1:("UP","green"),0:("DOWN","red")}),nodata="DOWN")
stat("Nodes reachable", f'sum(haify_controller_node_reachable{{{C}}}) / count(haify_controller_node_reachable{{{C}}})',4,y,4,4,unit="percentunit",th=steps(("red",None),("orange",0.5),("green",1)),nodata="—")
stat("Resources", f'count(haify_drbd_resource_up{{{C}}})',8,y,4,4,th=steps(("blue",None)))
stat("Degraded replicas", f'count(haify_drbd_disk_state{{{C},state!~"UpToDate|Diskless"}} == 1)',12,y,3,4,th=ok_bad)
stat("Quorum lost", f'count(haify_drbd_quorum{{{C}}} == 0)',15,y,3,4,th=ok_bad)
stat("Critical alerts", f'sum(haify_controller_alerts_firing{{{C},severity="critical"}})',18,y,3,4,th=ok_bad)
stat("Warnings", f'sum(haify_controller_alerts_firing{{{C},severity="warning"}})',21,y,3,4,th=steps(("green",None),("orange",1)))
y+=4
row("Resources",y); y+=1
table("Resources", 0, y, 24, 13, [
    tgt(f'max by (resource) (haify_drbd_resource_up{{{C}}})',"A",table=True),
    tgt(f'group by (resource, node) (haify_drbd_role{{{C},role="Primary"}} == 1)',"B",table=True),
    tgt(f'count by (resource) (haify_drbd_disk_state{{{C},state="UpToDate"}} == 1)',"C",table=True),
    tgt(f'count by (resource) (haify_drbd_disk_state{{{C},state!~"Diskless|DUnknown"}} == 1)',"D",table=True),
    tgt(f'min by (resource) (haify_drbd_quorum{{{C}}})',"E",table=True),
    tgt(f'sum by (resource) (haify_drbd_out_of_sync_bytes{{{C}}})',"F",table=True),
    tgt(f'min by (resource) (haify_drbd_connection_tls{{{C}}})',"G",table=True),
    tgt(f'group by (resource, domain) (haify_controller_resource_fault_domain_risk{{{C}}} == 1)',"H",table=True),
    tgt(f'group by (resource, degraded) (label_join(haify_drbd_disk_state{{{C},state!~"UpToDate|Diskless"}} == 1, "degraded", " ", "node", "state"))',"I",table=True),
  ], "resource",
  {"resource":"Resource","node":"Primary","Value #A":"Readable","Value #C":"UpToDate","Value #D":"Copies","Value #E":"Quorum","Value #F":"Out of sync","Value #G":"TLS","domain":"Single point of failure","degraded":"Degraded"},
  ["Resource","Primary","Readable","UpToDate","Copies","Quorum","Out of sync","TLS","Degraded","Single point of failure","Value #B","Value #H","Value #I"],
  [col("Value #B",[{"id":"custom.hidden","value":True}]),col("Value #H",[{"id":"custom.hidden","value":True}]),col("Value #I",[{"id":"custom.hidden","value":True}]),
   col("Degraded",[{"id":"noValue","value":" "},{"id":"custom.cellOptions","value":{"type":"color-text"}},{"id":"color","value":{"mode":"fixed","fixedColor":"red"}}]),
   col("Readable",colored({1:("yes","green"),0:("no","red")})),
   col("Quorum",colored({1:("yes","green"),0:("lost","red")})),
   col("TLS",colored({1:("on","green"),0:("off","text")})),
   col("Out of sync",[{"id":"unit","value":"bytes"},{"id":"thresholds","value":steps(("green",None),("orange",1))},{"id":"custom.cellOptions","value":{"type":"color-text"}}]),
   col("Single point of failure",[{"id":"noValue","value":" "},{"id":"custom.cellOptions","value":{"type":"color-text"}},{"id":"color","value":{"mode":"fixed","fixedColor":"orange"}}]),
   col("Primary",[{"id":"noValue","value":"none"}]),
   col("Resource",[{"id":"custom.width","value":300}]),
   col("Single point of failure",[{"id":"custom.width","value":180}]),
   *[col(n,[{"id":"custom.width","value":90}]) for n in ["Readable","UpToDate","Copies","Quorum","TLS"]]])
y+=13
row("Nodes",y); y+=1
table("Nodes", 0, y, 24, 6, [
    tgt(f'max by (node) (haify_controller_node_reachable{{{C}}})',"A",table=True),
    tgt(f'max by (node) (100 * haify_controller_storage_capacity_bytes{{{C},state="used"}} / on (cluster, pool, node) haify_controller_storage_capacity_bytes{{{C},state="total"}})',"B",table=True),
    tgt(f'max by (node) (haify_controller_pool_thin_used_percent{{{C},kind="metadata"}})',"C",table=True),
    tgt(f'count by (node) (haify_drbd_role{{{C},role="Primary"}} == 1)',"D",table=True),
  ], "node",
  {"node":"Node","Value #A":"Reachable","Value #B":"Pool used","Value #C":"Thin metadata","Value #D":"Primary for"},
  ["Node","Reachable","Pool used","Thin metadata","Primary for"],
  [col("Reachable",colored({1:("yes","green"),0:("no","red")})),
   col("Pool used",[{"id":"unit","value":"percent"},{"id":"min","value":0},{"id":"max","value":100},{"id":"thresholds","value":steps(("green",None),("orange",85),("red",95))},{"id":"custom.cellOptions","value":{"type":"gauge","mode":"basic"}}]),
   col("Thin metadata",[{"id":"unit","value":"percent"},{"id":"min","value":0},{"id":"max","value":100},{"id":"thresholds","value":steps(("green",None),("orange",85),("red",95))},{"id":"custom.cellOptions","value":{"type":"gauge","mode":"basic"}}]),
   col("Primary for",[{"id":"noValue","value":"0"},{"id":"unit","value":"none"}])])
y+=6
row("Trends",y); y+=1
ts("Pool use",[tgt(f'100 * haify_controller_storage_capacity_bytes{{{C},state="used"}} / on (cluster, pool, node) haify_controller_storage_capacity_bytes{{{C},state="total"}}',"A","{{node}} {{pool}}")],0,y,12,8,unit="percent",maxv=100,th=steps(("green",None),("orange",85),("red",95)))
ts("Alerts firing",[tgt(f'sum by (type, severity) (haify_controller_alerts_firing{{{C}}})',"A","{{severity}} {{type}}")],12,y,12,8,stack=True,nodata="Nothing firing")
y+=8
ts("Out of sync",[tgt(f'sum by (resource) (haify_drbd_out_of_sync_bytes{{{C}}}) > 0',"A","{{resource}}")],0,y,12,8,unit="bytes",nodata="Nothing out of sync")
ts("Controller API",[tgt(f'sum(rate(haify_controller_grpc_requests_total{{{C}}}[5m]))',"A","requests/s"),
                     tgt(f'histogram_quantile(0.95, sum by (le) (rate(haify_controller_grpc_request_duration_seconds_bucket{{{C}}}[5m])))',"B","p95 latency")],12,y,12,8,unit="reqps",
   overrides=[{"matcher":{"id":"byName","options":"p95 latency"},"properties":[{"id":"unit","value":"s"},{"id":"custom.axisPlacement","value":"right"}]}])
y+=8
row("Backups",y); y+=1
table("Backups", 0, y, 24, 6, [
    tgt(f'max by (resource, target) (haify_controller_backup_last_success_timestamp_seconds{{{C}}}) * 1000',"A",table=True),
    tgt(f'max by (resource, target) (haify_controller_backup_last_shipped_bytes{{{C}}})',"B",table=True),
  ], "resource",
  {"resource":"Resource","target":"Target","Value #A":"Last backup","Value #B":"Shipped"},
  ["Resource","Target","Last backup","Shipped"],
  [col("Last backup",[{"id":"unit","value":"dateTimeFromNow"}]),col("Shipped",[{"id":"unit","value":"bytes"}])], nodata="No backups yet")
y+=6
# Per node, from node_exporter (job="node", discovered from the controller).
N=f'job="node",{C}'
row("Nodes",y); y+=1
ts("Memory used",[tgt(f'100 * (1 - node_memory_MemAvailable_bytes{{{N}}} / node_memory_MemTotal_bytes{{{N}}})',"A","{{node}}")],0,y,12,8,unit="percent",maxv=100,th=steps(("green",None),("orange",85),("red",95)),nodata="No node_exporter targets")
ts("CPU busy",[tgt(f'100 * (1 - avg by (node) (rate(node_cpu_seconds_total{{{N},mode="idle"}}[5m])))',"A","{{node}}")],12,y,12,8,unit="percent",maxv=100,nodata="No node_exporter targets")
y+=8
ts("Disk I/O (DRBD devices)",[tgt(f'sum by (node) (rate(node_disk_written_bytes_total{{{N},device=~"drbd.*"}}[5m]))',"A","{{node}} write"),
                               tgt(f'sum by (node) (rate(node_disk_read_bytes_total{{{N},device=~"drbd.*"}}[5m]))',"B","{{node}} read")],0,y,12,8,unit="Bps",nodata="No DRBD I/O")
ts("Network",[tgt(f'sum by (node) (rate(node_network_transmit_bytes_total{{{N},device!~"lo|veth.*|docker.*|br-.*"}}[5m]))',"A","{{node}} out"),
              tgt(f'sum by (node) (rate(node_network_receive_bytes_total{{{N},device!~"lo|veth.*|docker.*|br-.*"}}[5m]))',"B","{{node}} in")],12,y,12,8,unit="Bps",nodata="No node_exporter targets")
y+=8
ts("Load (1 min) per CPU",[tgt(f'node_load1{{{N}}} / on (node) count by (node) (node_cpu_seconds_total{{{N},mode="idle"}})',"A","{{node}}")],0,y,12,8,unit="none",nodata="No node_exporter targets")
ts("Root filesystem used",[tgt(f'100 * (1 - node_filesystem_avail_bytes{{{N},mountpoint="/"}} / node_filesystem_size_bytes{{{N},mountpoint="/"}})',"A","{{node}}")],12,y,12,8,unit="percent",maxv=100,th=steps(("green",None),("orange",85),("red",95)),nodata="No node_exporter targets")
dash={"uid":"haify-overview","title":"Haify","tags":["haify"],"timezone":"browser","schemaVersion":41,"refresh":"30s",
 "time":{"from":"now-24h","to":"now"},"graphTooltip":1,
 "templating":{"list":[{"name":"cluster","label":"Cluster","type":"query","datasource":DS,
   "query":{"query":"label_values(up{job=\"haify\"}, cluster)","refId":"q"},"definition":"label_values(up{job=\"haify\"}, cluster)",
   "includeAll":False,"multi":False,"refresh":2,"sort":1}]},
 "panels":panels}
json.dump(dash,open(__import__('os').path.join(__import__('os').path.dirname(__file__),'dashboards','haify.json'),'w'),indent=1)
print(len(panels))
