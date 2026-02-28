package onvif

// SOAP XML response templates for ONVIF services.

const soapEnvelopeHeader = `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:tds="http://www.onvif.org/ver10/device/wsdl"
            xmlns:tt="http://www.onvif.org/ver10/schema"
            xmlns:trt="http://www.onvif.org/ver10/media/wsdl"
            xmlns:tev="http://www.onvif.org/ver10/events/wsdl"
            xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"
            xmlns:wsa="http://www.w3.org/2005/08/addressing"
            xmlns:tns1="http://www.onvif.org/ver10/topics">
  <s:Body>`

const soapEnvelopeFooter = `
  </s:Body>
</s:Envelope>`

const getDeviceInformationResponse = `
    <tds:GetDeviceInformationResponse>
      <tds:Manufacturer>HomeKit Proxy</tds:Manufacturer>
      <tds:Model>RTSP Bridge</tds:Model>
      <tds:FirmwareVersion>1.0.0</tds:FirmwareVersion>
      <tds:SerialNumber>%s</tds:SerialNumber>
      <tds:HardwareId>%s</tds:HardwareId>
    </tds:GetDeviceInformationResponse>`

const getCapabilitiesResponse = `
    <tds:GetCapabilitiesResponse>
      <tds:Capabilities>
        <tt:Device>
          <tt:XAddr>http://%s/onvif/device_service</tt:XAddr>
        </tt:Device>
        <tt:Media>
          <tt:XAddr>http://%s/onvif/media_service</tt:XAddr>
        </tt:Media>
        <tt:Events>
          <tt:XAddr>http://%s/onvif/event_service</tt:XAddr>
          <tt:WSPullPointSupport>true</tt:WSPullPointSupport>
        </tt:Events>
      </tds:Capabilities>
    </tds:GetCapabilitiesResponse>`

const getServicesResponse = `
    <tds:GetServicesResponse>
      <tds:Service>
        <tds:Namespace>http://www.onvif.org/ver10/device/wsdl</tds:Namespace>
        <tds:XAddr>http://%s/onvif/device_service</tds:XAddr>
        <tds:Version><tt:Major>2</tt:Major><tt:Minor>0</tt:Minor></tds:Version>
      </tds:Service>
      <tds:Service>
        <tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace>
        <tds:XAddr>http://%s/onvif/media_service</tds:XAddr>
        <tds:Version><tt:Major>2</tt:Major><tt:Minor>0</tt:Minor></tds:Version>
      </tds:Service>
      <tds:Service>
        <tds:Namespace>http://www.onvif.org/ver10/events/wsdl</tds:Namespace>
        <tds:XAddr>http://%s/onvif/event_service</tds:XAddr>
        <tds:Version><tt:Major>2</tt:Major><tt:Minor>0</tt:Minor></tds:Version>
      </tds:Service>
    </tds:GetServicesResponse>`

const getVideoSourcesResponse = `
    <trt:GetVideoSourcesResponse>
      <trt:VideoSources token="VS_1">
        <tt:Framerate>30</tt:Framerate>
        <tt:Resolution>
          <tt:Width>%d</tt:Width>
          <tt:Height>%d</tt:Height>
        </tt:Resolution>
      </trt:VideoSources>
    </trt:GetVideoSourcesResponse>`

const getProfilesResponse = `
    <trt:GetProfilesResponse>
      <trt:Profiles token="MainProfile" fixed="true">
        <tt:Name>MainProfile</tt:Name>
        <tt:VideoSourceConfiguration token="VSC_1">
          <tt:Name>VideoSource</tt:Name>
          <tt:UseCount>1</tt:UseCount>
          <tt:SourceToken>VS_1</tt:SourceToken>
          <tt:Bounds x="0" y="0" width="%d" height="%d"/>
        </tt:VideoSourceConfiguration>
        <tt:VideoEncoderConfiguration token="VEC_1">
          <tt:Name>H264</tt:Name>
          <tt:UseCount>1</tt:UseCount>
          <tt:Encoding>H264</tt:Encoding>
          <tt:Resolution>
            <tt:Width>%d</tt:Width>
            <tt:Height>%d</tt:Height>
          </tt:Resolution>
          <tt:RateControl>
            <tt:FrameRateLimit>%d</tt:FrameRateLimit>
            <tt:BitrateLimit>%d</tt:BitrateLimit>
          </tt:RateControl>
        </tt:VideoEncoderConfiguration>
      </trt:Profiles>
    </trt:GetProfilesResponse>`

const getVideoEncoderConfigurationResponse = `
    <trt:GetVideoEncoderConfigurationResponse>
      <trt:Configuration token="VEC_1">
        <tt:Name>H264</tt:Name>
        <tt:UseCount>1</tt:UseCount>
        <tt:Encoding>H264</tt:Encoding>
        <tt:Resolution>
          <tt:Width>%d</tt:Width>
          <tt:Height>%d</tt:Height>
        </tt:Resolution>
        <tt:RateControl>
          <tt:FrameRateLimit>%d</tt:FrameRateLimit>
          <tt:BitrateLimit>%d</tt:BitrateLimit>
        </tt:RateControl>
      </trt:Configuration>
    </trt:GetVideoEncoderConfigurationResponse>`

const getVideoEncoderConfigurationOptionsResponse = `
    <trt:GetVideoEncoderConfigurationOptionsResponse>
      <trt:Options>
        <tt:H264>
          <tt:ResolutionsAvailable>
            <tt:Width>%d</tt:Width>
            <tt:Height>%d</tt:Height>
          </tt:ResolutionsAvailable>
          <tt:FrameRateRange>
            <tt:Min>1</tt:Min>
            <tt:Max>%d</tt:Max>
          </tt:FrameRateRange>
          <tt:EncodingIntervalRange>
            <tt:Min>1</tt:Min>
            <tt:Max>1</tt:Max>
          </tt:EncodingIntervalRange>
          <tt:BitrateRange>
            <tt:Min>128</tt:Min>
            <tt:Max>%d</tt:Max>
          </tt:BitrateRange>
          <tt:H264ProfilesSupported>High</tt:H264ProfilesSupported>
        </tt:H264>
      </trt:Options>
    </trt:GetVideoEncoderConfigurationOptionsResponse>`

const getStreamUriResponse = `
    <trt:GetStreamUriResponse>
      <trt:MediaUri>
        <tt:Uri>%s</tt:Uri>
        <tt:InvalidAfterConnect>false</tt:InvalidAfterConnect>
        <tt:InvalidAfterReboot>false</tt:InvalidAfterReboot>
        <tt:Timeout>PT60S</tt:Timeout>
      </trt:MediaUri>
    </trt:GetStreamUriResponse>`

const createPullPointSubscriptionResponse = `
    <tev:CreatePullPointSubscriptionResponse>
      <tev:SubscriptionReference>
        <wsa:Address>http://%s/onvif/event_service/pullpoint/%s</wsa:Address>
      </tev:SubscriptionReference>
      <wsnt:CurrentTime>%s</wsnt:CurrentTime>
      <wsnt:TerminationTime>%s</wsnt:TerminationTime>
    </tev:CreatePullPointSubscriptionResponse>`

const pullMessagesResponse = `
    <tev:PullMessagesResponse>
      <tev:CurrentTime>%s</tev:CurrentTime>
      <tev:TerminationTime>%s</tev:TerminationTime>%s
    </tev:PullMessagesResponse>`

const notificationMessage = `
      <wsnt:NotificationMessage>
        <wsnt:Topic Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet">tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic>
        <wsnt:Message>
          <tt:Message UtcTime="%s" PropertyOperation="Changed">
            <tt:Source>
              <tt:SimpleItem Name="VideoSourceConfigurationToken" Value="VSC_1"/>
              <tt:SimpleItem Name="VideoAnalyticsConfigurationToken" Value="VAC_1"/>
              <tt:SimpleItem Name="Rule" Value="MyMotionDetectorRule"/>
            </tt:Source>
            <tt:Data>
              <tt:SimpleItem Name="IsMotion" Value="%s"/>
            </tt:Data>
          </tt:Message>
        </wsnt:Message>
      </wsnt:NotificationMessage>`

const getEventPropertiesResponse = `
    <tev:GetEventPropertiesResponse>
      <tev:TopicNamespaceLocation>http://www.onvif.org/ver10/topics/topicns.xml</tev:TopicNamespaceLocation>
      <wsnt:FixedTopicSet>true</wsnt:FixedTopicSet>
      <wstop:TopicSet xmlns:wstop="http://docs.oasis-open.org/wsn/t-1">
        <tns1:RuleEngine>
          <CellMotionDetector>
            <Motion wstop:topic="true">
              <tt:MessageDescription IsProperty="true">
                <tt:Source>
                  <tt:SimpleItemDescription Name="VideoSourceConfigurationToken" Type="tt:ReferenceToken"/>
                  <tt:SimpleItemDescription Name="VideoAnalyticsConfigurationToken" Type="tt:ReferenceToken"/>
                  <tt:SimpleItemDescription Name="Rule" Type="xs:string"/>
                </tt:Source>
                <tt:Data>
                  <tt:SimpleItemDescription Name="IsMotion" Type="xs:boolean"/>
                </tt:Data>
              </tt:MessageDescription>
            </Motion>
          </CellMotionDetector>
        </tns1:RuleEngine>
      </wstop:TopicSet>
      <wsnt:TopicExpressionDialect>http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet</wsnt:TopicExpressionDialect>
      <tev:MessageContentFilterDialect>http://www.onvif.org/ver10/tev/messageContentFilter/ItemFilter</tev:MessageContentFilterDialect>
    </tev:GetEventPropertiesResponse>`
