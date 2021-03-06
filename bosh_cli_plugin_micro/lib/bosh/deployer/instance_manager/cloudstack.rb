require 'bosh/deployer/registry'
require 'bosh/deployer/remote_tunnel'
require 'bosh/deployer/ssh_server'

module Bosh::Deployer
  class InstanceManager
    class Cloudstack
      def initialize(instance_manager, config, logger)
        @instance_manager = instance_manager
        @logger = logger
        @config = config
        properties = config.cloud_options['properties']

        @registry = Registry.new(
          properties['registry']['endpoint'],
          'cloudstack',
          properties['cloudstack'],
          instance_manager,
          logger,
        )

        ssh_key, ssh_port, ssh_user, ssh_wait = ssh_properties(properties)
        ssh_server = SshServer.new(ssh_user, ssh_key, ssh_port, logger)
        @remote_tunnel = RemoteTunnel.new(ssh_server, ssh_wait, logger)
      end

      def remote_tunnel
        @remote_tunnel.create(instance_manager.client_services_ip, registry.port)
      end

      def disk_model
        nil
      end

      def update_spec(spec)
        properties = spec.properties

        properties['cloudstack'] =
          config.spec_properties['cloudstack'] ||
          config.cloud_options['properties']['cloudstack'].dup

        properties['cloudstack']['registry'] = config.cloud_options['properties']['registry']
        properties['cloudstack']['stemcell'] = config.cloud_options['properties']['stemcell']

        spec.delete('networks')
      end

      def check_dependencies
        # nothing to check, move on...
      end

      def start
        registry.start
      end

      def stop
        registry.stop
        instance_manager.save_state
      end

      def client_services_ip
        logger.info('discovering client services ip')
        discover_client_services_ip
      end

      def agent_services_ip
        logger.info('discovering agent services ip')
        discover_agent_services_ip
      end

      def internal_services_ip
        config.internal_services_ip
      end

      # @return [Integer] size in MiB
      def disk_size(cid)
        # CloudStack stores disk size in GiB but we work with MiB
        instance_manager.cloud.compute.volumes.get(cid).size * 1024
      end

      def persistent_disk_changed?
        # since CloudStack stores disk size in GiB and we use MiB there
        # is a risk of conversion errors which lead to an unnecessary
        # disk migration, so we need to do a double conversion
        # here to avoid that
        requested = (config.resources['persistent_disk'] / 1024.0).ceil * 1024
        requested != disk_size(instance_manager.state.disk_cid)
      end

      private

      attr_reader :registry, :instance_manager, :logger, :config

      def ssh_properties(properties)
        ssh_user = properties['cloudstack']['ssh_user']
        ssh_port = properties['cloudstack']['ssh_port'] || 22
        ssh_wait = properties['cloudstack']['ssh_wait'] || 60

        key = properties['cloudstack']['private_key']
        err 'Missing properties.cloudstack.private_key' unless key
        ssh_key = File.expand_path(key)
        unless File.exists?(ssh_key)
          err "properties.cloudstack.private_key '#{key}' does not exist"
        end

        [ssh_key, ssh_port, ssh_user, ssh_wait]
      end

      def discover_client_services_ip
        if instance_manager.state.vm_cid
          server = instance_manager.cloud.compute.servers.get(instance_manager.state.vm_cid)

          floating_ip = instance_manager.cloud.compute.ipaddresses.find {
              |addr| addr.virtual_machine_id == server.id
          }

          private_ip_address = server.nics.first['ipaddress']

          ip = floating_ip.nil? ? private_ip_address : floating_ip.ip_address

          logger.info("discovered bosh ip=#{ip}")
          ip
        else
          default_ip = config.client_services_ip
          logger.info("ip address not discovered - using default of #{default_ip}")
          default_ip
        end
      end

      def discover_agent_services_ip
        if instance_manager.state.vm_cid
          server = instance_manager.cloud.compute.servers.get(instance_manager.state.vm_cid)

          ip = server.nics.first['ipaddress']

          logger.info("discovered bosh ip=#{ip}")
          ip
        else
          default_ip = config.agent_services_ip
          logger.info("ip address not discovered - using default of #{default_ip}")
          default_ip
        end
      end
    end
  end
end
