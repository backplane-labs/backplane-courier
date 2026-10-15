import { PluginRoutes } from '@backplane-labs/backplane-plugin-sdk';
import { definition } from './definition';
// The platform supplies router, clients, theme, translations and service context.
export default function Routes() { return <PluginRoutes definition={definition} />; }
