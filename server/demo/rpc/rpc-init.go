package rpc

import (
	"fmt"
	"github.com/kataras/iris"
	"github.com/peterq/pan-light/server/realtime"
	"github.com/pkg/errors"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

type gson = map[string]interface{}

type waitState struct {
	ticket  string           // 排队凭证
	order   int64            // 排序
	session realtime.Session // 用户会话
}

var server *realtime.Server
var manager = struct {
	inited bool

	hostSecret   map[string]string // host 名字, 秘钥表, 用于host认证
	hostMap      map[string]*roleHost
	hostMapLock  sync.RWMutex
	slaveMap     map[string]*roleSlave
	slaveMapLock sync.RWMutex
	userMap      map[realtime.SessionId]*roleUser
	userMapLock  sync.RWMutex

	waitSessionMap       map[int64]*waitState // 排队队列, key为order, 递增
	waitSessionMapLock   sync.RWMutex
	lastDistributedOrder int64 // 上次分配的序号
	lastInServiceOrder   int64 // 上次进入服务的序号
}{}

func Init(router iris.Party, hostSecret map[string]string) {
	if manager.inited {
		return
	}
	manager.inited = true
	manager.hostSecret = hostSecret
	manager.hostMap = map[string]*roleHost{}
	manager.waitSessionMap = map[int64]*waitState{}
	manager.userMap = map[realtime.SessionId]*roleUser{}
	server = &realtime.Server{
		SessionKeepTime:         10 * time.Second,
		KeepMessageCount:        32,
		BeforeAcceptSession:     onNewSession,
		BeforeDispatchUserEvent: eventFilter,
		BeforeDispatchUserRpc:   rpcFilter,
		OnSessionLost:           onSessionLost,
	}
	router.Any("/ws", iris.FromStd(server.HttpHandler()))
	server.RegisterEventHandler(userEventMap)
	server.RegisterRpcHandler(userRpcMap)
	server.RegisterEventHandler(hostEventMap)
	server.RegisterRpcHandler(hostRpcMap)
}

func onSessionLost(ss *realtime.Session) {
	role := ss.Data.(roleType)
	roleName := role.roleName()
	if roleName == "user" {
		onUserLeave(role.(*roleUser))
		return
	}
	if roleName == "host" {
		onHostLeave(role.(*roleHost))
		return
	}
	if roleName == "slave" {
		onSlaveLeave(role.(*roleSlave), ss)
	}

}

func onSlaveLeave(slave *roleSlave, ss *realtime.Session) {
	slave.lock.Lock()
	defer slave.lock.Unlock()
	// slave 会话每次结束演示会断开, 因此slave对象会重复使用, 这里需要检测Session一致性
	if slave.session != ss {
		return
	}
	slave.session = nil
	slave.userSession = nil
}

func onHostLeave(host *roleHost) {
	// 通知所有用户
	server.RoomByName("user_all").
		Broadcast("system.host.leave", host.name)
	// 取消注册
	manager.hostMapLock.Lock()
	defer manager.hostMapLock.Unlock()
	delete(manager.hostMap, host.name)
}

func onUserLeave(user *roleUser) {
	// 通知同房间的其他用户
	for _, room := range user.session.Rooms() {
		room.Broadcast("user.leave", user.session.Id())
	}
	// 踢出队列
	if user.waitState != nil {
		manager.waitSessionMapLock.Lock()
		defer manager.waitSessionMapLock.Unlock()
		delete(manager.waitSessionMap, user.waitState.order)
	}
}

func onNewSession(ss *realtime.Session) (err error) {
	defer func() {
		e := recover()
		if e != nil {
			err = errors.New(fmt.Sprint("roleType handshake error", e))
		}
	}()
	data, err := ss.Read()
	if err != nil {
		return
	}
	role := data["role"].(string)
	if role == "user" {
		return userVerify(data, ss)
	}
	if role == "host" {
		return hostVerify(data, ss)
	}
	if role == "slave" {
		return slaveVerify(data, ss)
	}
	return errors.New("roleType 不存在")
}

// user 会话认证
func userVerify(data gson, ss *realtime.Session) error {
	// 随机数回声, 防止有人用机器攻击
	i := rand.Intn(86400)
	ss.Emit("rand.check", i)
	ret, err := ss.Read()
	if err != nil {
		return errors.Wrap(err, "回声检测失败")
	}
	if i+1 != int(ret["rand.back"].(float64)) {
		log.Println(i+1, int(ret["rand.back"].(float64)))
		return errors.New("回声检测未通过")
	}
	manager.userMapLock.Lock()
	defer manager.userMapLock.Unlock()
	user := &roleUser{
		session:   ss,
		waitState: nil,
	}
	ss.Data = user
	manager.userMap[ss.Id()] = user
	return nil
}

// host会话认证
func hostVerify(data gson, ss *realtime.Session) error {
	name := data["host_name"].(string)
	secret := data["host_secret"].(string)
	correctSecret, ok := manager.hostSecret[name]
	if !ok {
		return errors.New("host 不存在")
	}
	if correctSecret != secret {
		return errors.New("秘钥错误")
	}
	manager.hostMapLock.Lock()
	defer manager.hostMapLock.Unlock()
	// 确保没有注册过
	host, ok := manager.hostMap[name]
	if ok {
		ss.Emit("error.register.already", host.session.Id())
		return errors.New("该host已经注册")
	} else {
		// 注册
		host = &roleHost{
			name:    name,
			session: ss,
		}
		ss.Data = host
		manager.hostMap[name] = host
	}
	return nil
}

// slave会话认证
func slaveVerify(data gson, ss *realtime.Session) error {
	hostName := data["host_name"].(string)
	secret := data["host_secret"].(string)
	slaveName := data["salve_name"].(string)
	correctSecret, ok := manager.hostSecret[hostName]
	if !ok {
		return errors.New("host 不存在")
	}
	if correctSecret != secret {
		return errors.New("秘钥错误")
	}
	if strings.Index(slaveName, hostName) != 0 { // 前缀检测
		return errors.New("forbidden")
	}
	manager.slaveMapLock.RLock()
	defer manager.slaveMapLock.RUnlock()
	slave, ok := manager.slaveMap[slaveName]
	slave.lock.Lock()
	defer slave.lock.Unlock()
	if !ok {
		return errors.New("slave 不存在")
	}
	if slave.session != nil {
		server.RemoveSession(slave.session.Id())
	}
	slave.session = ss
	return nil
}

// 事件权限检测
func eventFilter(ss *realtime.Session, event string) (err error) {
	role := ss.Data.(roleType)
	if strings.Index(event, role.roleName()+".") != 0 {
		return errors.New("forbidden")
	}
	return nil
}

// rpc权限检测
func rpcFilter(ss *realtime.Session, event string) (err error) {
	role := ss.Data.(roleType)
	if strings.Index(event, role.roleName()+".") != 0 {
		return errors.New("forbidden")
	}
	return nil
}
