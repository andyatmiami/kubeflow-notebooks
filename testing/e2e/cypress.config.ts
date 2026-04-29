import { defineConfig } from 'cypress';
import { registerK8sTasks } from './cypress/support/tasks/k8s-client';

export default defineConfig({
  e2e: {
    baseUrl: process.env.CYPRESS_BASE_URL || 'https://localhost:8443/workspaces',
    specPattern: 'cypress/tests/**/*.cy.ts',
    supportFile: 'cypress/support/e2e.ts',
    defaultCommandTimeout: 30_000,
    responseTimeout: 30_000,
    pageLoadTimeout: 60_000,
    chromeWebSecurity: false,
    video: true,
    videoCompression: 32,
    screenshotOnRunFailure: true,
    videosFolder: 'cypress/videos',
    screenshotsFolder: 'cypress/screenshots',
    retries: {
      runMode: 2,
      openMode: 0,
    },
    setupNodeEvents(on) {
      registerK8sTasks(on);

      on('after:spec', (_spec, results) => {
        if (results && results.video && results.stats.failures === 0) {
          const fs = require('fs');
          fs.unlinkSync(results.video);
        }
      });
    },
  },
});
