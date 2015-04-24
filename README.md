# statusmonitor
A simple web page status monitor

The sole purpose of monitor is to check if web pages respond with a code 200 OK.

# Requirements
Installed and correctly configured Go (golang.org). The tool was written under Go 1.4.

# Usage:

	go run statusmonitor.go -mode server -interval 10m -config config.json -workers 2

For a dozen of so URLs checked every 10 minutes one worker is fine. If you have a lot URLs to check or want to do it faster, just increase a number of workers.

# Modifying config through RPC call

As a service usually run a long time I recommend to use below command to add / remove URLs:
	
	go run statusmonitor.go -mode add -sname Olcamp -saddr http://olcamp.pl

Simple way to remove an address:

	go run statusmonitor.go -mode remove -sname Olcamp

Or below, using address

	go run statusmonitor.go -mode remove -saddr http://olcamp.pl
