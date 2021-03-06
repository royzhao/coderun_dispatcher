/*
*	get the stats of the servers and containers
 */

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	// "github.com/Sirupsen/logrus"
	"github.com/antonholmquist/jason"
	"github.com/fsouza/go-dockerclient"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// var logger = logrus.New()
var ContainerMemCapacity = float64(20971520) //default container memory capacity is 20MB
var l sync.RWMutex
var curClusterStats = make([]curServerStatus, 0, 5) //the global variable is current server stats and container stats
var DefaultContainerCapacify int = 100              //一个完全空闲容器能承担的用户数
var DefaultServerCapacity int = 1500                //一个完全空闲容器能承担的用户数

var curClusterLoad float64

type curServerStatus struct {
	machineStatus   serverStat      //the server stats
	containerStatus []containerStat // the container stats of this server

}

/*type containerAddr struct { // function dispatcherContainer will return this
	ServerIP   string
	ServerPost int
}*/
type serverConfig struct { // store data of ./metadata/config.json
	Server []struct {
		Host         string
		DockerPort   int
		CAdvisorPort int
	}
}

type serverAddress struct {
	Host         string
	DockerPort   int
	CAdvisorPort int
}

//对服务器状态和处理能力分离之后添加的数据结构
var UpdateStatChannel = make(chan updateInfo, 100) //缓冲区大小100，存放更新的服务器状态
type ContainerCapacity struct {
	host         string
	port         int
	containerID  string
	imageName    string
	capacityLeft int
}
type ServerCapacity struct {
	l            sync.RWMutex
	host         string
	CapacityLeft int
	containers   []ContainerCapacity
}

var curClusterCapacity = make([]ServerCapacity, 0, 5)

func getInitialServerAddr() serverConfig { // get default server info from ./metadata/config.json
	r, err := os.Open("../metadata/config.json")
	if err != nil {
		logger.Error(err)
	}
	decoder := json.NewDecoder(r)
	var c serverConfig
	err = decoder.Decode(&c)
	if err != nil {
		logger.Error(err)
	}
	// fmt.Println("初始服务器地址是", c)
	return c

}

type serverStat struct {
	Host         string
	DockerPort   int
	CAdvisorPort int

	cpuUsage        float64 //百分比
	cpuFrequencyKHz int64   //kHz
	cpuCore         int64   //核心数,-1表示服务器不在线

	memUsageTotal float64 //内存容量，单位为Byte
	memUsageHot   float64 //当前活跃内存量
	memCapacity   float64 //内存总量

	l sync.RWMutex
}

type containerStat struct {
	serverIP      string
	name          string  //image name
	id            string  //container id
	port          int     //暴露在外的端口
	cpuUsage      float64 //percent
	memUsageTotal float64 //Byte
	memeUsageHot  float64
	memCapacity   float64
}

type updateInfo struct { //服务器主动发送的机器信息
	Host       string
	Cpu        float64
	Mem        float64
	Containers []struct {
		Image string
		Id    string
		Cpu   float64
		Mem   float64
	}
}

var UpdateInfoChannel = make(chan updateInfo)

func subSubstring(str string, start, end int) string { //截取字符串
	if start < 0 {
		start = 0
	}
	if end > len(str) {

		// log.Println("数组越界，字符串长度是", len(str), "结尾长度是", end)
		end = len(str)
	}
	if start > len(str) {
		log.Println("数组越界，字符串长度", len(str), "开头长度是", start)
	}

	return string(str[start:end])
}

/*func serverStatSliceRemove(slice []serverStat, start, end int) []serverStat {
	return append(slice[:start], slice[end:]...)
}
func serverStatSliceRemoveAtIndex(slice []serverStat, index int) []serverStat {
	if index+1 >= len(slice) { // 末尾
		return slice[:index-1]
	} else if index-1 < 0 { //开头
		return slice[1:]
	} else {
		return serverStatSliceRemove(slice, index-1, index+1)
	}
}*/

func getServerStats(serverList serverConfig) []serverStat { // get current server stat

	su := make([]serverStat, 0, 10)
	for index := 0; index < len(serverList.Server); index++ {
		var temp serverStat
		su = append(su, temp)
		if serverList.Server[index].Host == "" {
			su[index].cpuCore = -1
			continue
		}

		// su[index].CPUUsage = "CPU" + strconv.Itoa(index)
		// su[index].MemUsage = "Mem" + strconv.Itoa(index)

		cadvisorUrl := "http://" + serverList.Server[index].Host + ":" + strconv.Itoa(serverList.Server[index].CAdvisorPort)
		posturl := cadvisorUrl + "/api/v1.0/containers"

		reqContent := "{\"num_stats\":2,\"num_samples\":0}"
		body := ioutil.NopCloser(strings.NewReader(reqContent))
		client := &http.Client{}
		req, errReq := http.NewRequest("POST", posturl, body)
		if errReq != nil {
			logger.Errorln("初始化错误", errReq)
			su[index].cpuCore = -1
			continue
		}
		resq, errResq := client.Do(req)
		if errResq != nil {
			logger.Errorln("初始化错误", errResq)
			su[index].cpuCore = -1
			continue
		}
		defer resq.Body.Close()
		data, errData := ioutil.ReadAll(resq.Body)
		if errData != nil {
			logger.Errorln("初始化错误", errData)
			su[index].cpuCore = -1
			continue
		}
		// fmt.Println(string(data), err)

		t, _ := jason.NewObjectFromBytes(data)
		stats, _ := t.GetObjectArray("stats") //从cAdvisor获取的最近两个stat,1是最新的
		// fmt.Println("len is ", len(stats))
		t1, _ := stats[1].GetString("timestamp")
		t2, _ := stats[0].GetString("timestamp")
		// fmt.Println("timestamp1 is ", t1, "the timestamp 2 is", t2)
		t1Time, _ := strconv.ParseFloat(subSubstring(t1, 17, 29), 64) //从秒开始，舍弃最后一个字母Z，不知道Z什么意思
		t2Time, _ := strconv.ParseFloat(subSubstring(t2, 17, 29), 64)
		// t2Time, _ := strconv.ParseFloat(subSubstring(t1, 17, 50), 64)
		// fmt.Println("t1 time is ", t1Time)
		intervalInNs := (t1Time - t2Time) * 1000000000 //单位是纳秒
		// fmt.Println("interval is ", intervalInNs)
		t1CPUUsage, _ := stats[1].GetFloat64("cpu", "usage", "total")
		t2CPUUsage, _ := stats[0].GetFloat64("cpu", "usage", "total")
		// fmt.Println("tiCPU is ", t1CPUUsage, t2CPUUsage)
		su[index].cpuUsage = (t1CPUUsage - t2CPUUsage) / intervalInNs

		if su[index].cpuUsage < 0 {
			su[index].cpuUsage = -su[index].cpuUsage
			// log.Println("CPU真复试", su[index].cpuUsage)

		}

		// ppp := GetCurrentClusterStatus()
		if intervalInNs < 1 && intervalInNs > -1 {
			su[index].cpuUsage = 0.01

			// cs[index].cpuUsage = ppp[0].machineStatus.cpuUsage
		}

		memoryUsageTotal, _ := stats[1].GetFloat64("memory", "usage")
		memoryUsageWorking, _ := stats[1].GetFloat64("memory", "working_set")
		su[index].memUsageTotal = memoryUsageTotal
		su[index].memUsageHot = memoryUsageWorking

		posturl2 := cadvisorUrl + "/api/v1.0/machine"
		client2 := &http.Client{}
		req2, errReq2 := http.NewRequest("POST", posturl2, nil)
		if errReq2 != nil {
			su[index].cpuCore = -1
			log.Println("req2出错，", errReq2)
			continue
		}
		resq2, errResq2 := client2.Do(req2)
		if errResq2 != nil {
			su[index].cpuCore = -1
			log.Println("resq2出错，", errResq2)
			continue
		}
		defer resq2.Body.Close()
		data2, errData2 := ioutil.ReadAll(resq2.Body)
		if errData2 != nil {
			su[index].cpuCore = -1
			log.Println("errData2出错", errData2)
			continue
		}
		tt2, errTt2 := jason.NewObjectFromBytes(data2)
		if errTt2 != nil {
			su[index].cpuCore = -1
			log.Println("errTT2出错", errTt2)
			continue
		}
		num_cores, errNum_cores := tt2.GetInt64("num_cores")
		if errNum_cores != nil {
			su[index].cpuCore = -1
			log.Println("errNum_cores出错", errNum_cores)
			continue
		}
		cpu_frequency_khz, errCpu_frequency := tt2.GetInt64("cpu_frequency_khz")
		if errCpu_frequency != nil {
			su[index].cpuCore = -1
			log.Println("errCpu_frequency出错", errCpu_frequency)
			continue
		}
		mem_capacity, errMem_capacity := tt2.GetFloat64("memory_capacity")
		if errMem_capacity != nil {
			su[index].cpuCore = -1
			log.Println("errMem_capacity出错", errMem_capacity)
			continue
		}
		su[index].cpuCore = num_cores
		su[index].cpuFrequencyKHz = cpu_frequency_khz
		su[index].memCapacity = mem_capacity
		su[index].Host = serverList.Server[index].Host
		su[index].CAdvisorPort = serverList.Server[index].CAdvisorPort
		su[index].DockerPort = serverList.Server[index].DockerPort
		su[index].cpuUsage = su[index].cpuUsage / float64(su[index].cpuCore)

	} // end of loop

	var temp []serverStat
	for _, v := range su {
		if v.cpuCore == -1 {
			continue
		}
		temp = append(temp, v)
	}
	return temp

}

func getValidContainerName(url string) []string { //返回字符串为docker名字,如/docker/0a69438e4……
	// reqContent := "{\"num_stats\":2,\"num_samples\":0}"
	// body := ioutil.NopCloser(strings.NewReader(reqContent))
	client := &http.Client{}
	req, _ := http.NewRequest("POST", url, nil)
	resq, _ := client.Do(req)
	defer resq.Body.Close()
	data, _ := ioutil.ReadAll(resq.Body)

	dataDecoded, _ := jason.NewObjectFromBytes(data)
	containerList, _ := dataDecoded.GetObjectArray("subcontainers")
	// var containerNameList []string
	containerNameList := make([]string, 0, 50)
	for i, v := range containerList {
		containerNameList = append(containerNameList, "")
		containerNameList[i], _ = v.GetString("name")
	}
	// containerNameList,_
	// fmt.Println("list is ", containerNameList)
	// return containerNameList
	return containerNameList
}

type HostAndIp struct {
	HostIp   string
	HostPort string
}

func getImageNameByContainerName(serverUrl string, containerName string) map[string]string { //返回内容为:镜像名称，容器对外端口
	// http://ip:port， docker/id
	id := subSubstring(containerName, 8, 100) //猜测不会超过100个字符，实际等同于从第8个字符开始截取
	// fmt.Println("name is ", containerName)
	// fmt.Println("id is ", id)
	// log.Println("截取后的id是", id)
	client, _ := docker.NewClient(serverUrl)
	// imageName,_ := client.InspectContainer
	containerInfo, err := client.InspectContainer(id)
	if err != nil {
		return make(map[string]string)
	}
	// fmt.Println("imgs is", containerInfo.Config.Image)
	//记录当前容器
	// log.Println("imgs is ", containerInfo.Config.Image)
	// temp := containerInfo.NetworkSettings.Ports["8080/tcp"]
	// temp := HostAndIp{"", ""}
	var temp []docker.PortBinding

	for _, v := range containerInfo.NetworkSettings.Ports {
		if v != nil {
			temp = v

			break
		}
	}
	if len(temp) == 0 {
		re := map[string]string{
			"ImageName":   containerInfo.Config.Image,
			"ExpostdPort": "",
		}
		return re
	}
	re := map[string]string{
		"ImageName":   containerInfo.Config.Image,
		"ExpostdPort": temp[0].HostPort,
		// containerInfo.NetworkSettings.Ports["8080/tcp"]["HostPort"]
	}

	// fmt.Println("怎么", temp[0].HostPort)
	return re
}
func getIntervalInNs(tPrevious string, tNext string) (float64, error) { //t1示例2015-05-21T11:18:47.723768816Z
	tPArray := strings.Split(tPrevious, ":")
	tNArray := strings.Split(tNext, ":")
	if len(tPArray) != 3 {
		return 0, errors.New("第一个参数格式错误")
	} else if len(tNArray) != 3 {
		return 0, errors.New("第二个参数格式错误")
	}
	// re := float64(0)
	tPMinute, errTPM := strconv.Atoi(tPArray[1])
	tNMinute, errTNM := strconv.Atoi(tNArray[1])
	tPSecond, errTPS := strconv.ParseFloat(strings.Replace(tPArray[2], "Z", "", -1), 64)
	tNSecond, errTNS := strconv.ParseFloat(strings.Replace(tNArray[2], "Z", "", -1), 64)
	if errTPM != nil || errTNM != nil || errTPS != nil || errTNS != nil {
		return 0, errors.New("数据转换出错")
	}
	intervalMinute := tNMinute - tPMinute
	if intervalMinute == 0 {
		return 1000000000 * (tNSecond - tPSecond), nil
	} else {
		return (1000000000) * (float64(intervalMinute*60) + tNSecond - tPSecond), nil
	}

}
func getContainerStat(serverIP string, cadvisorPort int, dockerPort int, ContainerNameList []string) []containerStat {
	//serverUrl format is: http://server_ip:port
	cs := make([]containerStat, 0, 50)
	serverUrl := "http://" + serverIP + ":" + strconv.Itoa(cadvisorPort)
	for index := 0; index < len(ContainerNameList); index++ {
		var temp containerStat
		posturl := serverUrl + "/api/v1.0/containers" + ContainerNameList[index]
		// fmt.Println("posturl is ", posturl)
		// continue
		reqContent := "{\"num_stats\":2,\"num_samples\":0}"
		body := ioutil.NopCloser(strings.NewReader(reqContent))
		client := &http.Client{}
		req, errReq := http.NewRequest("POST", posturl, body)
		if errReq != nil {
			logger.Errorln("初始化错误", errReq)
			temp.cpuUsage = -1
			continue
		}
		resq, errResq := client.Do(req)
		defer resq.Body.Close()
		if errResq != nil {
			logger.Errorln("请求错误错误", errResq)
			temp.cpuUsage = -1
			continue
		}
		data, _ := ioutil.ReadAll(resq.Body)

		// fmt.Println("test")
		t, _ := jason.NewObjectFromBytes(data)
		// fmt.Println("t是神马", data)
		stats, _ := t.GetObjectArray("stats") //从cAdvisor获取的最近两个stat,1是最新的
		// fmt.Println("len is ", len(stats))
		if len(stats) < 2 {
			log.Println("状态长度不够", len(stats))
			index = index - 1
			continue
		}
		t1, _ := stats[1].GetString("timestamp")
		t2, _ := stats[0].GetString("timestamp")
		// fmt.Println("test")
		// fmt.Println("timestamp1 is ", t1, "the timestamp 2 is", t2)
		// t1Time, _ := strconv.ParseFloat(subSubstring(t1, 17, 29), 64) //从秒开始，舍弃最后一个字母Z，不知道Z什么意思
		// t2Time, _ := strconv.ParseFloat(subSubstring(t2, 17, 29), 64)
		// intervalInNs := (t1Time - t2Time) * 1000000000 //单位是纳秒
		intervalInNs, errIIN := getIntervalInNs(t2, t1)
		if errIIN != nil {
			log.Println("时间间隔获取错误")
		}
		// fmt.Println("test")
		// fmt.Println("interval is ", intervalInNs)
		t1CPUUsage, _ := stats[1].GetFloat64("cpu", "usage", "total")
		t2CPUUsage, _ := stats[0].GetFloat64("cpu", "usage", "total")
		// fmt.Println("tiCPU is ", t1CPUUsage, t2CPUUsage)
		temp.cpuUsage = (t1CPUUsage - t2CPUUsage) / intervalInNs
		if temp.cpuUsage < 0 {
			temp.cpuUsage = -temp.cpuUsage
			// log.Println("CPU真复试", cs[index].cpuUsage)

		}

		// ppp := GetCurrentClusterStatus()
		if intervalInNs < 1 && intervalInNs > -1 {
			temp.cpuUsage = 0.01

			// cs[index].cpuUsage = ppp[0].machineStatus.cpuUsage
		}
		// log.Println("时间间隔是", intervalInNs)
		// log.Println("第一个时间", t1Time, "第二个是", t2Time)
		memoryUsageTotal, _ := stats[1].GetFloat64("memory", "usage")
		memoryUsageWorking, _ := stats[1].GetFloat64("memory", "working_set")

		temp.memUsageTotal = memoryUsageTotal
		temp.memeUsageHot = memoryUsageWorking
		temp.memCapacity = ContainerMemCapacity
		temp.serverIP = serverIP
		temp.id = subSubstring(ContainerNameList[index], 8, 20)
		// if len(ContainerNameList) < index {
		// 	log.Println("ContainerNameList长度是", len(ContainerNameList), "index是", index)
		// 	log.Println("容器id是", temp.id)
		// }
		// log.Println("名字是", ContainerNameList[index])
		// cs[index].name = getImageNameByContainerName(cs[index].serverAddr, ContainerNameList[index])
		iif := getImageNameByContainerName("http://"+temp.serverIP+":"+"4243", ContainerNameList[index])
		// cs[index].name = iif["ImageName"]
		tttt := strings.Split(iif["ImageName"], "/")
		if len(tttt) > 1 {
			temp.name = tttt[1]
		} else {
			temp.name = iif["ImageName"]
		}
		// cs[index].name = strings.Split(iif["ImageName"], "/")
		tempPort, _ := (strconv.Atoi(iif["ExpostdPort"]))
		temp.port = int(tempPort)
		/*		fmt.Println("serverip is ", cs[index].serverIP)
				fmt.Println("image name is ", iif["ImageName"])
				fmt.Println("container id is ", cs[index].id)
				fmt.Println("container port is ", cs[index].port)*/
		// cs[index].serverAddr = serverUrl
		// fmt.Println("container cpu is ", cs[index].cpuUsage)
		if temp.cpuUsage > 0.99 {
			log.Println("CPU出错,id是", temp.id, "用量是", temp.cpuUsage, "t1是", t1CPUUsage, "t2是", t2CPUUsage, "时间是", intervalInNs)
			log.Println("t1是", t1, "t2是", t2)
			log.Println("状态0是", stats[0])
			log.Println("状态1是", stats[1])
		}
		cs = append(cs, temp)

	} // end of loop
	// fmt.Println("containerstat is gotten")
	// fmt.Println("len is ", len(ContainerNameList))
	return cs

}

func GetCurrentClusterStatus() []curServerStatus { // return current curClusterStatus
	l.RLock()
	defer l.RUnlock()
	// log.Println("读锁")
	return curClusterStats
}
func SetCurrentClusterStatus(newStat []curServerStatus) {
	l.Lock()
	defer l.Unlock()
	// log.Println("写锁")
	curClusterStats = curClusterStats[0:0]
	curClusterStats = append(curClusterStats, newStat...)
	return
}

func findServerByHost(info updateInfo) (int, error) {
	// curCluster := GetCurrentClusterStatus()
	// l.RLock()
	// defer l.RUnlock()
	for i, v := range curClusterCapacity {
		v.l.RLock()
		if v.host == info.Host {
			return i, nil
		}
		v.l.RUnlock()
	}
	return -1, errors.New("没有对应的主机")
}

func CalculateContainerCapacity(containersStat []containerStat) []ContainerCapacity {
	tempContainerCapa := make([]ContainerCapacity, len(containersStat))
	// fmt.Println("长度是", len(containersStat))
	if len(containersStat) < 1 {
		fmt.Println("没有容器")
		return make([]ContainerCapacity, 0)
	}
	for i, v := range containersStat {
		tempContainerCapa[i].imageName = v.name
		tempContainerCapa[i].containerID = v.id
		tempContainerCapa[i].host = v.serverIP
		tempContainerCapa[i].port = v.port
		if v.memUsageTotal > v.memCapacity { //只有系统应用才有可能超过20MB的限制
			tempContainerCapa[i].capacityLeft = -1
			continue
		}
		memUsage := v.memUsageTotal / v.memCapacity
		if v.cpuUsage > memUsage {
			tempContainerCapa[i].capacityLeft = int(math.Floor(float64(DefaultContainerCapacify) * (1 - v.cpuUsage)))
		} else {
			tempContainerCapa[i].capacityLeft = int(math.Floor(float64(DefaultContainerCapacify) * (1 - memUsage)))
		}
		if v.cpuUsage > 1 || memUsage > 1 {
			log.Println("出错了,", v)
			log.Println("CPU是", v.cpuUsage, "内存是", memUsage)
			log.Println("内存用量", v.memUsageTotal, "内存容量", v.memCapacity)
		}
	} //循环结束
	return tempContainerCapa
}

func StartDeamon() { // load the initial server info from ./metadata/config.json
	// and update server and container status periodicly
	servers := getInitialServerAddr()

	// fmt.Println("servers is ", servers)
	// getImageNameByContainerName("http://192.168.0.33:4243", "/docker/0a69438e4d780629c9c8ef2b672d9aea03ccaf1b7b56dd97458174e59e47618c")
	timeSlot := time.NewTimer(time.Second * 1) // update status every second
	for {
		select {
		case <-timeSlot.C:
			//TODO the codes to update
			// fmt.Println(serverSStats)
			a := time.Now()
			serverSStats := getServerStats(servers) //因为服务器可能 发生 在线\不在线的变化
			// tempClusterStats := curClusterStats[0:0]
			// tempClusterStats := GetCurrentClusterStatus()[0:0]
			tempClusterStats := make([]curServerStatus, 0, 1)
			tempClusterCapacity := make([]ServerCapacity, 0, 1)
			for index := 0; index < len(serverSStats); index++ {
				aa := time.Now()
				var temp curServerStatus
				tempClusterStats = append(tempClusterStats, temp)
				if serverSStats[index].cpuCore == -1 { //由于getServerStats中已经有判断，这里没有必要
					tempClusterStats[index].machineStatus.cpuCore = -1
					continue
				}
				//获取服务器状态
				// cs := getServerStats(serverSStats[index].Host + strconv.Itoa(serverSStats[index].CAdvisorPort))
				var tempServerConfig serverConfig
				var tempServerAddress serverAddress
				// tempServerConfig.Server = append(tempServerConfig.Server, tempServerAddress)
				tempServerConfig.Server = append(tempServerConfig.Server, tempServerAddress)
				// fmt.Println("长度是 ", len(tempServerConfig.Server))

				tempServerConfig.Server[0].Host = string(serverSStats[index].Host)
				tempServerConfig.Server[0].CAdvisorPort = int(serverSStats[index].CAdvisorPort)
				tempServerConfig.Server[0].DockerPort = int(serverSStats[index].DockerPort)

				var ss []serverStat
				ss = getServerStats(tempServerConfig)
				tempClusterStats[index].machineStatus = ss[0]

				//获取容器名字
				serverUrl := "http://" + serverSStats[index].Host + ":" + strconv.Itoa(serverSStats[index].CAdvisorPort) + "/api/v1.0/containers/docker"
				// fmt.Println("url is ", serverUrl)
				containerNames := getValidContainerName(serverUrl)
				// fmt.Println("container names is", containerNames)
				// cs := getContainerStat("http://"+serverSStats[index].Host+":"+strconv.Itoa(serverSStats[index].CAdvisorPort), containerNames)
				cs := getContainerStat(serverSStats[index].Host, serverSStats[index].CAdvisorPort, serverSStats[index].DockerPort, containerNames)
				// fmt.Println("containers is ", cs)
				// log.Println("更新容器状态是", cs)
				tempClusterStats[index].containerStatus = append(tempClusterStats[index].containerStatus, cs...)

				//添加对服务器和容器处理能力的更新

				var tempServerCapa ServerCapacity
				tempServerCapa.host = tempServerConfig.Server[0].Host
				memUsage := tempClusterStats[index].machineStatus.memUsageTotal / tempClusterStats[index].machineStatus.memCapacity
				var serverCapa int
				if memUsage > tempClusterStats[index].machineStatus.cpuUsage {
					serverCapa = int(math.Floor(float64(DefaultServerCapacity) * (1 - memUsage)))
				} else {
					serverCapa = int(math.Floor(float64(DefaultServerCapacity) * (1 - tempClusterStats[index].machineStatus.cpuUsage)))
				}
				tempServerCapa.CapacityLeft = serverCapa
				tempServerCapa.containers = append(tempServerCapa.containers, CalculateContainerCapacity(tempClusterStats[index].containerStatus)...)
				tempClusterCapacity = append(tempClusterCapacity, tempServerCapa)
				// tempServerCapa.containers
				fmt.Println("服务器容器更新耗时", time.Now().Sub(aa))

			}
			fmt.Println("耗时", time.Now().Sub(a))
			SetCurrentClusterStatus(tempClusterStats) //加上了读写锁
			l.Lock()
			curClusterCapacity = tempClusterCapacity
			log.Println("更新后集群容量", curClusterCapacity)
			l.Unlock()
			//记录集群状态
			// log.Println("更新集群状态是", tempClusterStats)
			log.Println("集群状态是", GetCurrentClusterStatus())
			// fmt.Println("当前状态是 ", curClusterStats)
			// timeSlot.Reset(time.Second * 2)
			fmt.Println("更新状态耗时", time.Now().Sub(a))
			timeSlot.Reset(time.Second * 5)
		}
	}

}
func getUpdateInfo(w http.ResponseWriter, enc Encoder, r *http.Request) (int, string) { //接收信息，放入channel
	var receiveInfo updateInfo
	if err := json.NewDecoder(r.Body).Decode(&receiveInfo); err != nil {
		logger.Warnf("error decoding receiveInfo: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		fmt.Println("接收数据错误", err)
		return http.StatusBadRequest, Must(enc.Encode(err))
	}
	if receiveInfo.Host == "" {
		receiveInfo.Host = r.RemoteAddr
		temp := strings.Split(receiveInfo.Host, ":")
		if len(temp) > 0 {
			receiveInfo.Host = temp[0]
		}
	}
	fmt.Println("更新信息是", receiveInfo)
	UpdateInfoChannel <- receiveInfo
	return http.StatusOK, Must(enc.Encode(""))
}

func UpdateClusterCapacity() { //每次更新一个服务器中的信息
	stat := <-UpdateInfoChannel //取出一个更新信息
	hostIndex, err := findServerByHost(stat)
	if err != nil {
		log.Println("找不到主机,更新信息是", stat)
		return
	}
	for _, v := range stat.Containers {
		memUsage := v.Mem / ContainerMemCapacity
		var Capacity int
		if v.Cpu > memUsage {
			Capacity = int(math.Floor(float64(DefaultContainerCapacify) * (1 - v.Cpu)))
		} else {
			Capacity = int(math.Floor(float64(DefaultContainerCapacify) * (1 - v.Mem)))
		}
		for ii, vv := range curClusterCapacity[hostIndex].containers {
			if v.Id == vv.containerID {
				curClusterCapacity[hostIndex].l.Lock()
				curClusterCapacity[hostIndex].containers[ii].capacityLeft = Capacity
				curClusterCapacity[hostIndex].l.Unlock()
			}
		}
	}

}
func UpdateCurrentClusterStatus(newStat updateInfo) error {
	// tempStat:=

	log.Println("修改前集群状态是", curClusterStats)

	// tempStat := curClusterStats
	hostIndex, err := findServerByHost(newStat)
	if err != nil { //找不到对应的主机
		return errors.New("未找到对应的主机")
	}
	l.Lock()
	curClusterStats[hostIndex].machineStatus.l.Lock()
	defer curClusterStats[hostIndex].machineStatus.l.Unlock()
	l.Unlock()
	fmt.Println("...")
	curClusterStats[hostIndex].machineStatus.cpuUsage = newStat.Cpu
	curClusterStats[hostIndex].machineStatus.memUsageTotal = newStat.Mem
	for i, v := range curClusterStats[hostIndex].containerStatus {
		for _, vv := range newStat.Containers {
			if vv.Id == v.id {
				curClusterStats[hostIndex].containerStatus[i].cpuUsage = vv.Cpu
				curClusterStats[hostIndex].containerStatus[i].memUsageTotal = vv.Mem
			}
		}
	}
	// curClusterStats = tempStat
	// l.Lock()
	// // curClusterStats = tempStat
	// l.Unlock()
	log.Println("修改后集群状态是", curClusterStats)

	return nil
}
