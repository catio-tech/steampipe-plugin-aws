install: build
	mkdir -p ~/.steampipe/config
	echo 'connection "aws" {' > ~/.steampipe/config/aws.spc
	echo '  plugin = "local/aws"' >> ~/.steampipe/config/aws.spc
	echo '}' >> ~/.steampipe/config/aws.spc

build:
	go build -o ~/.steampipe/plugins/local/aws/aws.plugin *.go

docker:
	docker build --no-cache -t steampipe-plugin-aws .