# Ansible

## Commands
```shell
ansible-playbook -i inventory.yaml [--private-key=/path/to/your/key] deploy_ssh_pub_keys.yaml
ansible-playbook -i inventory.yaml [--private-key=/path/to/your/key] deploy_nginx_conf.yaml
ansible-playbook -i inventory.yaml [--private-key=/path/to/your/key] deploy_mysql_conf.yaml
ansible-playbook -i inventory.yaml [--private-key=/path/to/your/key] reboot_all.yaml
```