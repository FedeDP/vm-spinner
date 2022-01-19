package main

import (
	"context"
	"fmt"
	"github.com/jasondellaluce/experiments/vm-spinner/vmjobs"
	"os"
	"runtime"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sync/semaphore"
)

func defaultMemory() int {
	return 1024
}

func defaultParallelism() int {
	return runtime.NumCPU() / 2
}

func defaultNumCPUs() int {
	return runtime.NumCPU() / defaultParallelism()
}

func main() {
	app := cli.NewApp()
	app.Name = "vm-spinner"
	app.Usage = "Run your workloads on ephemeral Virtual Machines"
	app.Commands = []cli.Command{
		{
			Name:   "bpf",
			Usage:  "Run bpf build + verifier job.",
			Action: runApp,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "commithash",
					Usage: "falcosecurity/libs commit hash to run the test against.",
				},
			},
		},
		{
			Name:   "kmod",
			Usage:  "Run kmod build job.",
			Action: runApp,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "commithash",
					Usage: "falcosecurity/libs commit hash to run the test against.",
				},
			},
		},
		{
			Name:   "cmd",
			Usage:  "Run a simple cmd line job.",
			Action: runApp,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "line",
					Usage: "command that runs in each VM, as a command line parameter.",
				},
			},
		},
		{
			Name:   "stdin",
			Usage:  "Run a simple cmd line job read from stdin.",
			Action: runApp,
		},
		{
			Name:   "script",
			Usage:  "Run a simple script job read from file.",
			Action: runApp,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "file",
					Usage: "script that runs in each VM, as a filepath.",
				},
			},
		},
	}
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "images,i",
			Usage: "Comma-separated list of the VM image names to run the command on. Some jobs provide a default set of images (eg: bpf, kmod).",
		},
		cli.StringFlag{
			Name:  "provider,p",
			Usage: "Vagrant provider name.",
			Value: "virtualbox",
		},
		cli.IntFlag{
			Name:  "memory",
			Usage: "The amount of memory (in bytes) allocated for each VM.",
			Value: defaultMemory(),
		},
		cli.IntFlag{
			Name:  "cpus",
			Usage: "The number of cpus allocated for each VM.",
			Value: defaultNumCPUs(),
		},
		cli.IntFlag{
			Name:  "parallelism",
			Usage: "The number of VM to spawn in parallel.",
			Value: defaultParallelism(),
		},
		cli.BoolFlag{
			Name:  "log.json",
			Usage: "Whether to log output in json format.",
		},
		cli.StringFlag{
			Name:  "log.level",
			Usage: "Log level, between { trace, debug, info }.",
			Value: "debug",
		},
		cli.StringFlag{
			Name:  "log.output",
			Usage: "Log output filename. If empty, stdout will be used.",
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func validateParameters(c *cli.Context) error {
	if c.GlobalInt("cpus") > runtime.NumCPU() {
		return fmt.Errorf("number of CPUs for each VM (%d) exceeds the number of CPUs available (%d)", c.Int("cpus"), runtime.NumCPU())
	}

	if c.GlobalInt("parallelism") > runtime.NumCPU() {
		return fmt.Errorf("number of parallel VMs (%d) exceeds the number of CPUs available (%d)", c.Int("parallelism"), runtime.NumCPU())
	}

	if c.GlobalInt("parallelism")*c.GlobalInt("cpus") > runtime.NumCPU() {
		fmt.Printf("warning: number of parallel cpus (cpus * parallelism %d) exceeds the number of CPUs available (%d)\n", c.Int("parallelism")*c.Int("cpus"), runtime.NumCPU())
	}
	return nil
}

func initLog(c *cli.Context) error {
	// Log as JSON instead of the default ASCII formatter.
	if c.GlobalBool("log.json") {
		log.SetFormatter(&log.JSONFormatter{})
	}

	out := os.Stdout
	if len(c.GlobalString("log.output")) > 0 {
		var err error
		out, err = os.OpenFile(c.GlobalString("log-output"), os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return err
		}
	}
	log.SetOutput(out)

	switch c.GlobalString("log.level") {
	case "trace":
		log.SetLevel(log.TraceLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	default:
		log.SetLevel(log.DebugLevel)
	}
	return nil
}

func runApp(c *cli.Context) error {
	err := validateParameters(c)
	if err != nil {
		return err
	}

	err = initLog(c)
	if err != nil {
		return err
	}

	job, err := vmjobs.NewVMJob(c)
	if err != nil {
		log.Fatal(err)
	}

	// Goroutine to handle result in job plugin
	var resWg sync.WaitGroup
	resCh := make(chan vmjobs.VMOutput)
	resWg.Add(1)
	go func() {
		for res := range resCh {
			job.Process(res)
		}
		resWg.Done()
	}()

	// prepare sync primitives.
	// the waitgrup is used to run all the VM in parallel, and to
	// join with each worker goroutine once their job is finished.
	// the semapthore is used to ensure that the parallelism upper
	// limit gets respected.
	var wg sync.WaitGroup
	sm := semaphore.NewWeighted(int64(c.GlobalInt("parallelism")))

	// iterate through all the specified VM images
	images := job.Images()
	log.Infof("Running on %v images", images)
	for i, image := range images {
		wg.Add(1)
		sm.Acquire(context.Background(), 1)

		// launch the VM for this image
		name := fmt.Sprintf("/tmp/%s-%d", image, i)
		conf := &VMConfig{
			Name:         name,
			BoxName:      image,
			ProviderName: c.GlobalString("provider"),
			CPUs:         c.GlobalInt("cpus"),
			Memory:       c.GlobalInt("memory"),
			Command:      job.Cmd(),
		}

		// worker goroutine
		go func() {
			defer func() {
				sm.Release(1)
				wg.Done()
			}()

			// select the VM outputs
			channels := RunVirtualMachine(conf)
			for {
				logger := log.WithFields(log.Fields{"vm": conf.BoxName})
				select {
				case <-channels.Done:
					logger.Info("Job Finished.")
					return
				case l := <-channels.CmdOutput:
					logger.Info(l)
					resCh <- vmjobs.VMOutput{VM: conf.BoxName, Line: l}
				case l := <-channels.Debug:
					logger.Trace(l)
				case l := <-channels.Info:
					logger.Debug(l)
				case err := <-channels.Error:
					logger.Error(err.Error())
				}
			}
		}()
	}

	// wait for all workers
	wg.Wait()

	// Close summary matrix channel and wait
	// for it to eventually print the summary
	close(resCh)
	resWg.Wait()

	// Notify job that we're done
	job.Done()

	return nil
}
