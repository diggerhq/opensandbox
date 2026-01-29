export class Commands {
    static snapshot(name: string): string {
      return `vm snapshot create --name ${name}`;
    }
    
    static restart(service: string): string {
      return `systemctl restart ${service}`;
    }
    
    static readonly STATUS = "systemctl status";
  }
  
