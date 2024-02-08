<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ Backend tests faster than unit-tests, from user traffic ⚡️
</b>
</h3 >
<p align="center">
🌟 The must-have tool for developers in the AI-Gen era 🌟
</p>
---

<h4 align="center">

  <a href="CODE_OF_CONDUCT.md" alt="Contributions welcome">
    <img src="https://img.shields.io/badge/Contributions-Welcome-brightgreen?logo=github" /></a>
  <a href="https://github.com/keploy/keploy/actions" alt="Tests">
  <a href="https://goreportcard.com/report/github.com/keploy/keploy" alt="Go Report Card">
    <img src="https://goreportcard.com/badge/github.com/keploy/keploy" /></a>
  <a href="https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA" alt="Slack">
    <img src=".github/slack.svg" /></a>
  <a href="https://keploy.io/docs" alt="Docs">
    <img src=".github/docs.svg" /></a>
   <a href="https://github.com/Keploy/Keploy/blob/main/LICENSE">
    <img src="https://img.shields.io/badge/Licence-Apache-blue" alt="Keploy is released under the Apache License">
  </a>
  <a href="https://keploy.io/"><img src="https://img.shields.io/website?url=https://keploy.io/&up_message=Keploy&up_color=%232635F1&label=Accelerator&down_color=%232635F1&down_message=Keploy"></a>
  <a href="https://github.com/keploy/keploy/releases">
    <img title="Release" src="https://img.shields.io/github/v/release/keploy/keploy?logo=github"/>
  </a>
  <a href="https://github.com/Keploy/Keploy/releases">
    <img title="Release date" src="https://img.shields.io/github/release-date/Keploy/Keploy?logo=github"/>
  </a>
  <a href="https://github.com/Keploy/Keploy/graphs/contributors">
    <img title="Contributors" src="https://img.shields.io/github/contributors/Keploy/Keploy?logo=github"/>
  </a>
  <a href="https://github.com/Keploy/Keploy/pulls?q=is%3Apr+is%3Aclosed">
    <img title="Pull Requests" src="https://img.shields.io/github/issues-pr-closed/Keploy/Keploy?logo=github"/>
  </a>
  <a href="https://github.com/Keploy/Keploy/pulls?q=is%3Apr+is%3Aclosed">
    <img title="Release Build" src="https://img.shields.io/github/actions/workflow/status/Keploy/Keploy/release.yml?logo=github&label=Release Build"/>
  </a>
  <a href="https://github.com/Keploy/Keploy/blob/main/CONTRIBUTING.md">
    <img src="https://img.shields.io/badge/PRs-Welcome-brightgreen?logo=github" alt="PRs welcome!" />
  </a>
  <a href="https://github.com/Keploy/Keploy/issues">
    <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help us reach 4k stars! Now at:" alt="Help us reach 1k stars!" />
  </a>
  <a href="https://Keploy.io/docs">
    <img src="https://img.shields.io/badge/Join-Community!-orange" alt="Join our Community!" />
  </a>
  
  <a href="https://twitter.com/Keploy_io">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=twitter&style=social" alt="Keploy Twitter" />
  </a>
</h4>

## 🎤 Presentando Keploy 🐰
Keploy es una herramienta de prueba de backend centrada en el **desarrollador**. Realiza pruebas de backend con **mocks incorporados**, más rápido que las pruebas unitarias, a partir del tráfico del usuario, lo que lo hace **fácil de usar, potente y extensible**. 🛠

¿Listo para la magia? Aquí están las características principales de Keploy:

- ♻️ **Cobertura de prueba combinada:** Fusiona tus pruebas de Keploy con tus bibliotecas de pruebas favoritas (junit, go-test, py-test, jest) para ver una cobertura de prueba combinada.

- 🤖 **Instrumentación EBPF:** Keploy utiliza EBPF como un ingrediente secreto para hacer que la integración sea sin código, independiente del lenguaje y muy ligera.

- 🌐 **Integración CI/CD:** Ejecuta pruebas con mocks donde quieras, ya sea localmente en la CLI, en tu canal de integración continua o incluso en un clúster de Kubernetes. ¡Es prueba donde la necesitas!

- 🎭 **Mocks multipropósito:** Úsalos en pruebas existentes, como pruebas de servidor o simplemente para impresionar a tus amigos.

- 📽️ **Grabación y reproducción de flujos complejos:** Keploy puede grabar y reproducir flujos de API complejos y distribuidos como mocks y stubs. Es como tener una máquina del tiempo para tus pruebas, ¡ahorrándote mucho tiempo!

![Generar caso de prueba a partir de una llamada API](https://raw.githubusercontent.com/keploy/docs/main/static/gif/how-keploy-works.gif)

> 🐰 **Dato curioso:** ¡Keploy se utiliza a sí mismo para realizar pruebas! Echa un vistazo a nuestra elegante insignia de cobertura: [![Estado de cobertura](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🌐 Soporte de idiomas
Desde el gopher de Go 🐹 hasta la serpiente de Python 🐍, ofrecemos soporte para:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🎩 ¿Cómo funciona la magia?
Nuestro mágico 🧙‍♂️ proxy de Keploy captura y reproduce **TODAS** las interacciones de red de tu aplicación (operaciones CRUD, incluyendo APIs no idempotentes).

Realiza un viaje a **[¿Cómo funciona Keploy?](https://docs.keploy.io/docs/keploy-explained/how-keploy-works)** para descubrir los trucos detrás del telón.

![Generar caso de prueba a partir de una llamada API](https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif)

## 📘 ¡Aprende más!
Conviértete en un profesional de Keploy con nuestra **[Documentación](https://docs.keploy.io/)**.

# Instalación rápida

Usando **Binario** (<img src="https://th.bing.com/th/id/R.7802b52b7916c00014450891496fe04a?rik=r8GZM4o2Ch1tHQ&riu=http%3a%2f%2f1000logos.net%2fwp-content%2fuploads%2f2017%2f03%2fLINUX-LOGO.png&ehk=5m0lBvAd%2bzhvGg%2fu4i3%2f4EEHhF4N0PuzR%2fBmC1lFzfw%3d&risl=&pid=ImgRaw&r=0" width="20" height="20"> Linux</img> / <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="20" height="20"> WSL</img>)
-

Keploy se puede utilizar en Linux nativamente y a través de WSL en Windows.

### Descarga el binario de Keploy.

```zsh
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_amd64.tar.gz" | tar xz -C /tmp

sudo mkdir -p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin && keploy

<details>
<summary> <strong> Arquitectura ARM </strong> </summary>
curl --silent --location "https://github.com/keploy/keploy/releases/latest/download/keploy_linux_arm64.tar.gz" | tar xz -C /tmp

sudo mkdir-p /usr/local/bin && sudo mv /tmp/keploy /usr/local/bin && keploy
</details>

### Captura de casos de prueba
Para iniciar la grabación de llamadas a la API, ejecuta este comando en tu terminal donde normalmente ejecutas tu aplicación. Si necesitas configurar variables de entorno, hazlo de la manera habitual:

```zsh
sudo -E env PATH=$PATH keploy record -c "CMD_PARA_EJECUTAR_LA_APP"
```

Por ejemplo, si estás utilizando un programa sencillo de Golang, el comando se vería así:

```zsh
sudo -E env PATH=$PATH keploy record -c "CMD_PARA_EJECUTAR_LA_APP"
```

### Ejecución de casos de prueba

Para ejecutar los casos de prueba y generar un informe de cobertura de pruebas, utiliza este comando en la terminal donde normalmente ejecutas tu aplicación. Si necesitas configurar variables de entorno, hazlo de la manera habitual:

```zsh  
sudo -E env PATH=$PATH keploy test -c "CMD_PARA_EJECUTAR_LA_APP" --delay 10
 ```

 Por ejemplo, si estás utilizando un framework de Golang, el comando sería:

 ```zsh
 sudo -E env PATH=$PATH keploy test -c "go run main.go" --delay 10
 ```

<img src="https://cdn4.iconfinder.com/data/icons/logos-and-brands/512/97_Docker_logo_logos-512.png" width="20" height="20"> Instalación de Docker </img>

Keploy se puede utilizar en <img src="https://th.bing.com/th/id/R.7802b52b7916c00014450891496fe04a?rik=r8GZM4o2Ch1tHQ&riu=http%3a%2f%2f1000logos.net%2fwp-content%2fuploads%2f2017%2f03%2fLINUX-LOGO.png&ehk=5m0lBvAd%2bzhvGg%2fu4i3%2f4EEHhF4N0PuzR%2fBmC1lFzfw%3d&risl=&pid=ImgRaw&r=0" width="10" height="10"> Linux</img> y <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="10" height="10"> Windows</img> a través de Docker.

> **️ Nota:** <img src="https://www.pngplay.com/wp-content/uploads/3/Apple-Logo-Transparent-Images.png" width="15" height="15"> MacOS</img> necesitan instalar [Colima](https://github.com/abiosoft/colima#installation). <img src="https://cdn.freebiesupply.com/logos/large/2x/microsoft-windows-22-logo-png-transparent.png" width="10" height="10"/>Usuarios de Windows necesitan installar [WSL](https://learn.microsoft.com/en-us/windows/wsl/install#install-wsl-command).

### Creación de alias

Creemos un alias para Keploy:

```shell
alias keploy='sudo docker run --pull always --name keploy-v2 -p 16789:16789 --privileged --pid=host -it -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'
```

### Grabación de Casos de Prueba y Datos Simulados

Aquí tienes algunos puntos a considerar antes de la grabación:
- Si estás ejecutando mediante **docker-compose**, asegúrate de incluir el `<NOMBRE_DEL_CONTENEDOR>` en el servicio de tu aplicación en el archivo docker-compose.yaml [como se muestra aquí](https://github.com/keploy/samples-python/blob/9d6cf40da2eb75f6e035bedfb30e54564785d5c9/flask-mongo/docker-compose.yml#L14).
- Debes ejecutar los contenedores en una red, si no es así, asegúrate de que todos tus contenedores estén en la misma red con la propiedad externa activada - [como se muestra aquí](https://github.com/keploy/samples-python/blob/9d6cf40da2eb75f6e035bedfb30e54564785d5c9/flask-mongo/docker-compose.yml#L24). Reemplaza el **nombre de la red** (bandera `--network`) por tu red personalizada si la cambiaste anteriormente, como la red <backend> en el ejemplo dado.
- `Docker_CMD_to_run_user_container` se refiere al **comando de Docker para iniciar** la aplicación.

Utiliza el alias de keploy que creamos para capturar casos de prueba. **Ejecuta** el siguiente comando dentro del **directorio raíz** de tu aplicación.

```shell
keploy record -c "Docker_CMD_to_run_user_container --network <network_name>" --containerName "<container_name>"
```

Realiza llamadas API utilizando herramientas como [Hoppscotch](https://hoppscotch.io/), [Postman](https://www.postman.com/) o comandos cURL.

Keploy capturará las llamadas API que hayas realizado, generando suites de pruebas que comprenden **casos de prueba (KTests) y simulaciones de datos (KMocks)** en formato `YAML`.

### Ejecución de Casos de Prueba

Ahora, utiliza el alias keployV2 que creamos para ejecutar los casos de prueba. Sigue estos pasos en el **directorio raíz** de tu aplicación.

Cuando utilices **docker-compose** para iniciar la aplicación, es importante asegurarse de que el parámetro `--containerName` coincida con el nombre del contenedor en tu archivo `docker-compose.yaml`.

```shell
keploy test -c "Docker_CMD_to_run_user_container --network <network_name>" --containerName "<container_name>" --delay 20
```

## 🤔 Preguntas?
¡Contáctanos! Estamos aquí para ayudarte.

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-12rfbvc01-o54cOG0X1G6eVJTuI_orSA)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![Twitter](https://img.shields.io/badge/Twitter-%231DA1F2.svg?style=for-the-badge&logo=Twitter&logoColor=white)](https://twitter.com/Keployio)

## 💖 ¡Construyamos Juntos!
Ya seas un principiante o un mago 🧙‍♀️ en la programación, tu perspectiva es valiosa. Echa un vistazo a nuestras:

📜 [Directrices de Contribución](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Código de Conducta](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)

## 🌟 Características

### **🚀 Exporta, mantiene y muestra pruebas y simulaciones!**

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="90%"  alt="Genera Casos de Prueba desde Llamadas API"/>

### **🤝 Saluda a los populares frameworks de pruebas - Go-Test, JUnit, Py-Test, Jest y más!**

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/replay-tc.gif" width="90%"  alt="Genera Casos de Prueba desde Llamadas API"/>

### **🕵️ Detecta ruido con precisión de cirujano!**
Filtra campos ruidosos en las respuestas de las API como (marcas de tiempo, valores aleatorios) para asegurar pruebas de alta calidad.

### **📊 ¡Saluda a una mayor cobertura!**
Keploy se asegura de que no se generen casos de prueba redundantes.

## 🐲 Los Desafíos que Enfrentamos!
- **Pruebas Unitarias:** Aunque Keploy está diseñado para funcionar junto con los marcos de pruebas unitarias (Go test, JUnit, etc.) y puede contribuir a la cobertura de código general, todavía genera pruebas de extremo a extremo (E2E).
- **Entornos de Producción:** Keploy actualmente se centra en generar pruebas para desarrolladores. Estas pruebas se pueden capturar desde cualquier entorno, pero no las hemos probado en entornos de producción de alto volumen. Esto requeriría una sólida deduplicación para evitar la captura de pruebas redundantes en exceso. Tenemos ideas para construir un sistema de deduplicación sólido [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Recursos!
🤔 [Preguntas Frecuentes](https://docs.keploy.io/docs/keploy-explained/faq)

🕵️‍️ [¿Por Qué Keploy?](https://docs.keploy.io/docs/keploy-explained/why-keploy)

⚙️ [Guía de Instalación](https://docs.keploy.io/docs/server/server-installation)

📖 [Guía de Contribución](https://docs.keploy.io/docs/devtools/server-contrib-guide/)

## 🌟 Salón de Contribuyentes
<p>
  <img src="https://api.vaunt.dev/v1/github/entities/keploy/repositories/keploy/contributors?format=svg&limit=18" width="100%"  alt="contribuyentes"/>
</p>

### Premios Disponibles

| Nombre | Icono | Descripción |
| ---- | ---- | ----------- |
| Creador de Documentos | <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/docs_hero.png" width="150" alt="icono-de-docs" /> | ¡Premiado por ayudar a mejorar la documentación de Keploy! |
| Cada Bit Cuenta | <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/commit_hero.png" width="150" alt="icono-de-commit"/> | ¡Ningún commit es demasiado pequeño! |
| Héroe de Solicitudes de Extracción | <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/pull_request_hero.png" width="150" alt="icono-de-PR-hero" /> | ¡Eres un héroe de solicitudes de extracción, sigue así! |
| Cercano| <img src="https://raw.githubusercontent.com/sonichigo/keploy/main/.vaunt/badge/closer.png" width="150" alt="icono-de-closer" /> | ¡Solo los cercanos consiguen café! |
