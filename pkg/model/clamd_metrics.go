package model

type ClamMetrics struct {
	Pools   int        `json:"pools"`
	State   ClamdState `json:"state"`
	Threads struct {
		Live        int `json:"live"`
		Idle        int `json:"idle"`
		Max         int `json:"max"`
		IdleTimeout int `json:"idle_timeout"`
	} `json:"threads"`
	QueueLength int `json:"queue"`
	MemStats    struct {
		Heap       float64 `json:"heap"`
		Mmap       float64 `json:"mmap"`
		Used       float64 `json:"used"`
		Free       float64 `json:"free"`
		Releasable float64 `json:"releasable"`
		Pools      int     `json:"pools"`
		PoolsUsed  float64 `json:"pools_used"`
		PoolsTotal float64 `json:"pools_total"`
	} `json:"memstats"`
}

func (c *ClamMetrics) SetPools(v int) { c.Pools = v }

func (c *ClamMetrics) SetState(s ClamdState) { c.State = s }

func (c *ClamMetrics) SetThreads(live, idle, max, idleTimeout int) {
	c.Threads.Live = live
	c.Threads.Idle = idle
	c.Threads.Max = max
	c.Threads.IdleTimeout = idleTimeout
}

func (c *ClamMetrics) SetQueueLength(v int) { c.QueueLength = v }

func (c *ClamMetrics) SetMemStats(heap, mmap, used, free, releasable float64, pools int, poolsUsed, poolsTotal float64) {
	c.MemStats.Heap = heap
	c.MemStats.Mmap = mmap
	c.MemStats.Used = used
	c.MemStats.Free = free
	c.MemStats.Releasable = releasable
	c.MemStats.Pools = pools
	c.MemStats.PoolsUsed = poolsUsed
	c.MemStats.PoolsTotal = poolsTotal
}
