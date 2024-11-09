MAKE=make -C

DOCKER_BUILD=docker build
DOCKER_BUILD_OPTS=--no-cache
DOCKER_RMI=docker rmi -f

ISUPIPE_TAG=isupipe:latest

test: test_benchmarker
.PHONY: test

test_benchmarker:
	$(MAKE) bench test
.PHONY: test_benchmarker

build_webapp:
	$(MAKE) webapp/go docker_image
.PHONY: build_webapp

deploy-mysql:
	cd ansible && ansible-playbook -i inventory.yaml deploy_mysql_conf.yaml

deploy-nginx:
	cd ansible && ansible-playbook -i inventory.yaml deploy_nginx_conf.yaml

deploy-webapp:
	cd ansible && ansible-playbook -i inventory.yaml deploy_webapp.yaml
