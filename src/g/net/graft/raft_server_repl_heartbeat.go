// 数据同步需要注意的是：
// leader只有在通知完所有follower更新完数据之后，自身才会进行数据更新
// 因此leader
package graft

import (
    "net"
    "g/encoding/gjson"
    "time"
    "log"
    "g/core/types/gset"
    "sync"
)

// 用以识别节点当前是否正在数据同步中
var isInReplication bool

// leader到其他节点的数据同步监听
func (n *Node) logAutoReplicationHandler() {
    var wg sync.WaitGroup
    // 初始化数据同步心跳检测
    go n.logAutoReplicationLoop()
    // 日志自动清理
    go n.autoCleanLogList()
    // 进入循环监听日志事件
    for {
        select {
            case entry := <- n.LogChan:
                n.setStatusInReplication(true)
                log.Println("sending log entry", entry)
                for _, v := range n.Peers.Values() {
                    info := v.(NodeInfo)
                    if info.Status != gSTATUS_ALIVE {
                        continue
                    }
                    conn := n.getConn(info.Ip, gPORT_REPL)
                    if conn != nil {
                        wg.Add(1)
                        go func(conn net.Conn, entry LogEntry) {
                            if err := n.sendMsg(conn, entry.Act, *gjson.Encode(entry)); err != nil {
                                log.Println(err)
                                conn.Close()
                                wg.Done()
                                return
                            }
                            n.receiveMsg(conn)
                            wg.Done()
                        }(conn, entry)
                    }
                }
                wg.Wait()
                // 当所有节点的请求处理后，再保存数据到自身
                // 以便leader与follower之间的数据同步判断
                n.saveLogEntry(entry)
                n.setStatusInReplication(false)
        }
    }
}

// 日志自动同步检查，类似心跳
func (n *Node) logAutoReplicationLoop() {
    conns := gset.NewStringSet()
    for {
        if n.getRole() == gROLE_LEADER {
            ips := n.Peers.Keys()
            for _, ip := range ips {
                if conns.Contains(ip) {
                    continue
                }
                conn := n.getConn(ip, gPORT_REPL)
                if conn == nil {
                    conns.Remove(ip)
                    continue
                }
                conns.Add(ip)
                go func(ip string, conn net.Conn) {
                    defer func() {
                        conn.Close()
                        conns.Remove(ip)
                    }()
                    for {
                        // 如果当前正在数据同步操作中，那么等待
                        for n.getStatusInReplication() {
                            time.Sleep(100 * time.Millisecond)
                        }
                        if n.getRole() != gROLE_LEADER || !n.Peers.Contains(ip){
                            return
                        }
                        //log.Println("sending replication heartbeat to", ip)
                        if err := n.sendMsg(conn, gMSG_REPL_HEARTBEAT, ""); err != nil {
                            log.Println(err)
                            return
                        }
                        msg := n.receiveMsg(conn)
                        if msg != nil {
                            switch msg.Head {
                                case gMSG_REPL_NEED_UPDATE_FOLLOWER:
                                    n.updateDataToRemoteNode(conn, msg)

                                default:
                                    time.Sleep(gLOG_REPL_TIMEOUT_HEARTBEAT * time.Millisecond)
                            }
                        }
                    }
                }(ip, conn)
            }
        }
        time.Sleep(100 * time.Millisecond)
    }
}

// 定期清理已经同步完毕的日志列表
func (n *Node) autoCleanLogList() {
    for {
        time.Sleep(gLOG_REPL_LOGCLEAN_INTERVAL * time.Millisecond)
        if n.getRole() == gROLE_LEADER {
            minLogId := n.getMinLogIdFromPeers()
            p := n.LogList.Back()
            for p != nil {
                entry := p.Value.(LogEntry)
                if entry.Id <= minLogId {
                    t := p.Prev()
                    n.LogList.Remove(p)
                    p  = t
                } else {
                    break;
                }
            }
        }
    }
}

// 获取节点中已同步的最小的log id
func (n *Node) getMinLogIdFromPeers() int64 {
    var minLogId int64
    for _, v := range n.Peers.Values() {
        info := v.(NodeInfo)
        if minLogId == 0 || info.LastLogId < minLogId {
            minLogId = info.LastLogId
        }
    }
    return minLogId
}

// 根据logid获取还未更新的日志列表
func (n *Node) getLogEntriesByLastLogId(id int64) []LogEntry {
    if n.getLastLogId() > id {
        array := make([]LogEntry, 0)
        n.LogList.RLock()
        l := n.LogList.L.Back()
        for l != nil {
            r := l.Value.(LogEntry)
            if r.Id > id {
                array = append(array, r)
            }
            l = l.Prev()
        }
        n.LogList.RUnlock()
        return array
    }
    return nil
}